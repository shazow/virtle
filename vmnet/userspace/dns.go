package userspace

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/miekg/dns"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

const (
	dnsPort = 53
	// dnsTTL is what forwarded answers carry: the host resolver hides the
	// upstream TTL, so a short one keeps guests close to the host's view.
	dnsTTL = 60
	// dnsTimeout bounds one host lookup.
	dnsTimeout = 5 * time.Second
	// dnsEDNSSize is the UDP payload the gateway advertises.
	dnsEDNSSize = 4096
)

// resolver is the part of net.Resolver the gateway uses, so tests can
// answer without the host's DNS.
type resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
	LookupCNAME(ctx context.Context, host string) (string, error)
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
	LookupNS(ctx context.Context, name string) ([]*net.NS, error)
	LookupSRV(ctx context.Context, service, proto, name string) (string, []*net.SRV, error)
	LookupTXT(ctx context.Context, name string) ([]string, error)
	LookupAddr(ctx context.Context, addr string) ([]string, error)
}

// dnsServer answers guest queries on the gateway address over UDP and TCP.
type dnsServer struct {
	n        *Network
	resolver resolver
	udp, tcp *dns.Server
	udpConn  *gonet.UDPConn
	tcpLn    *gonet.TCPListener
}

func (n *Network) startDNS() error {
	var wq waiter.Queue
	ep, err := n.stack.NewEndpoint(udp.ProtocolNumber, ipv4.ProtocolNumber, &wq)
	if err != nil {
		return tcpipError("dns endpoint", err)
	}
	if err := ep.Bind(tcpip.FullAddress{NIC: nicID, Addr: n.gateway4, Port: dnsPort}); err != nil {
		ep.Close()
		return tcpipError("dns bind", err)
	}
	s := &dnsServer{n: n, resolver: net.DefaultResolver, udpConn: gonet.NewUDPConn(&wq, ep)}
	ln, lerr := gonet.ListenTCP(n.stack, tcpip.FullAddress{NIC: nicID, Addr: n.gateway4, Port: dnsPort}, ipv4.ProtocolNumber)
	if lerr != nil {
		_ = s.udpConn.Close()
		return lerr
	}
	s.tcpLn = ln
	s.udp = &dns.Server{PacketConn: s.udpConn, Handler: s}
	s.tcp = &dns.Server{Listener: ln, Handler: s}
	n.dns = s
	n.wg.Add(2)
	go func() {
		defer n.wg.Done()
		_ = s.udp.ActivateAndServe()
	}()
	go func() {
		defer n.wg.Done()
		_ = s.tcp.ActivateAndServe()
	}()
	return nil
}

// shutdown stops both servers. Closing the conns as well covers a server
// that has not started serving yet, which Shutdown would miss.
func (s *dnsServer) shutdown() {
	_ = s.udp.Shutdown()
	_ = s.tcp.Shutdown()
	_ = s.udpConn.Close()
	_ = s.tcpLn.Close()
}

// ServeDNS implements dns.Handler.
func (s *dnsServer) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	m := new(dns.Msg)
	m.SetReply(r)
	m.RecursionAvailable = true
	if len(r.Question) != 1 {
		m.Rcode = dns.RcodeFormatError
	} else {
		s.answer(m, r.Question[0])
	}
	size := dns.MinMsgSize
	if o := r.IsEdns0(); o != nil {
		m.SetEdns0(dnsEDNSSize, false)
		if int(o.UDPSize()) > size {
			size = int(o.UDPSize())
		}
	}
	if w.RemoteAddr().Network() == "udp" {
		m.Truncate(size)
	}
	_ = w.WriteMsg(m)
}

func (s *dnsServer) answer(m *dns.Msg, q dns.Question) {
	if q.Qclass != dns.ClassINET {
		m.Rcode = dns.RcodeNotImplemented
		return
	}
	ctx, cancel := context.WithTimeout(s.n.ctx, dnsTimeout)
	defer cancel()
	hdr := dns.RR_Header{Name: q.Name, Rrtype: q.Qtype, Class: dns.ClassINET, Ttl: dnsTTL}
	switch q.Qtype {
	case dns.TypeA:
		addrs, err := s.resolver.LookupNetIP(ctx, "ip4", q.Name)
		if err != nil {
			s.fail(m, err)
			return
		}
		for _, a := range addrs {
			if a = a.Unmap(); a.Is4() {
				m.Answer = append(m.Answer, &dns.A{Hdr: hdr, A: a.AsSlice()})
			}
		}
	case dns.TypeAAAA:
		// The segment carries IPv4 only; an empty answer sends the client
		// to its A query without claiming the name does not exist.
	case dns.TypeCNAME:
		cname, err := s.resolver.LookupCNAME(ctx, q.Name)
		if err != nil {
			s.fail(m, err)
			return
		}
		if cname = dns.Fqdn(cname); !strings.EqualFold(cname, q.Name) {
			m.Answer = append(m.Answer, &dns.CNAME{Hdr: hdr, Target: cname})
		}
	case dns.TypeMX:
		records, err := s.resolver.LookupMX(ctx, q.Name)
		if err != nil {
			s.fail(m, err)
			return
		}
		for _, mx := range records {
			m.Answer = append(m.Answer, &dns.MX{Hdr: hdr, Preference: mx.Pref, Mx: dns.Fqdn(mx.Host)})
		}
	case dns.TypeNS:
		records, err := s.resolver.LookupNS(ctx, q.Name)
		if err != nil {
			s.fail(m, err)
			return
		}
		for _, ns := range records {
			m.Answer = append(m.Answer, &dns.NS{Hdr: hdr, Ns: dns.Fqdn(ns.Host)})
		}
	case dns.TypeSRV:
		_, records, err := s.resolver.LookupSRV(ctx, "", "", q.Name)
		if err != nil {
			s.fail(m, err)
			return
		}
		for _, srv := range records {
			m.Answer = append(m.Answer, &dns.SRV{Hdr: hdr, Priority: srv.Priority, Weight: srv.Weight, Port: srv.Port, Target: dns.Fqdn(srv.Target)})
		}
	case dns.TypeTXT:
		records, err := s.resolver.LookupTXT(ctx, q.Name)
		if err != nil {
			s.fail(m, err)
			return
		}
		for _, txt := range records {
			m.Answer = append(m.Answer, &dns.TXT{Hdr: hdr, Txt: splitTXT(txt)})
		}
	case dns.TypePTR:
		addr, ok := reverseName(q.Name)
		if !ok {
			m.Rcode = dns.RcodeNameError
			return
		}
		if s.n.subnet.Contains(addr) {
			// Guest and gateway addresses have no names to give out.
			m.Rcode = dns.RcodeNameError
			return
		}
		names, err := s.resolver.LookupAddr(ctx, addr.String())
		if err != nil {
			s.fail(m, err)
			return
		}
		for _, name := range names {
			m.Answer = append(m.Answer, &dns.PTR{Hdr: hdr, Ptr: dns.Fqdn(name)})
		}
	default:
		m.Rcode = dns.RcodeNotImplemented
	}
}

// fail maps a resolver error to a response code.
func (s *dnsServer) fail(m *dns.Msg, err error) {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		m.Rcode = dns.RcodeNameError
		return
	}
	m.Rcode = dns.RcodeServerFailure
}

// reverseName parses "d.c.b.a.in-addr.arpa." into a.b.c.d.
func reverseName(name string) (netip.Addr, bool) {
	const suffix = ".in-addr.arpa."
	name = strings.ToLower(name)
	if !strings.HasSuffix(name, suffix) {
		return netip.Addr{}, false
	}
	parts := strings.Split(strings.TrimSuffix(name, suffix), ".")
	if len(parts) != 4 {
		return netip.Addr{}, false
	}
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	addr, err := netip.ParseAddr(strings.Join(parts, "."))
	return addr, err == nil && addr.Is4()
}

// splitTXT breaks a record into the 255-byte strings the wire format holds.
func splitTXT(s string) []string {
	const max = 255
	var parts []string
	for len(s) > max {
		parts = append(parts, s[:max])
		s = s[max:]
	}
	return append(parts, s)
}
