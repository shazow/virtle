package userspace

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/miekg/dns"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"

	"github.com/shazow/virtle/vm"
	"github.com/shazow/virtle/vmnet"
)

const (
	dnsPort = 53
	// dnsTTL is the lifetime advertised for synthetic answers.
	dnsTTL = 60
	// dnsEDNSSize is the UDP payload the gateway advertises.
	dnsEDNSSize = 4096
)

// dnsServer answers guest queries on the gateway address over UDP and TCP.
type dnsServer struct {
	n        *Network
	udp, tcp *dns.Server
	udpConn  *dnsPacketConn
	tcpLn    *dnsListener
	inflight chan struct{}
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
	udpEP := &dnsUDPEndpoint{Endpoint: ep}
	s := &dnsServer{n: n, udpConn: &dnsPacketConn{UDPConn: gonet.NewUDPConn(&wq, udpEP), n: n, ep: udpEP}, inflight: make(chan struct{}, 128)}
	ln := &dnsListener{addr: &net.TCPAddr{IP: net.IP(n.gateway.AsSlice()), Port: dnsPort}, conns: make(chan net.Conn), done: make(chan struct{})}
	s.tcpLn = ln
	s.udp = &dns.Server{PacketConn: s.udpConn, Handler: s}
	s.tcp = &dns.Server{Listener: ln, Handler: s, MaxTCPQueries: -1}
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
	started := time.Now()
	m := new(dns.Msg)
	m.SetReply(r)
	m.RecursionAvailable = true
	src, _ := netip.ParseAddrPort(w.RemoteAddr().String())
	var p *port
	if addr, ok := w.RemoteAddr().(*dnsAddr); ok {
		p = addr.p
	}
	var err error
	var guest, name string
	var qtype uint16
	if len(r.Question) != 1 || r.Opcode != dns.OpcodeQuery || r.Response {
		m.Rcode = dns.RcodeFormatError
		err = errors.New("expected one DNS query")
	} else if p == nil || p.isClosed() {
		m.Rcode, err = dns.RcodeRefused, vmnet.ErrDenied
	} else {
		q := r.Question[0]
		guest, name, qtype = p.name, fakeName(q.Name), q.Qtype
		if q.Name == "." {
			name = "."
		}
		f := vmnet.Flow{Guest: guest, Src: src, Host: name, Egress: p.egress, Proto: vm.Proto(w.RemoteAddr().Network())}
		err = s.answer(p.ctx, m, q, f)
		if err != nil {
			m.Rcode = dns.RcodeServerFailure
			if errors.Is(err, vmnet.ErrDenied) {
				m.Rcode = dns.RcodeRefused
			}
		}
	}
	// OPT is hop-specific. Only advertise the gateway's own capabilities,
	// never the upstream's cookies, options, or UDP size.
	m.Extra = slices.DeleteFunc(m.Extra, func(rr dns.RR) bool { return rr.Header().Rrtype == dns.TypeOPT })
	if r.IsEdns0() == nil && m.Rcode > 15 {
		m.Rcode = dns.RcodeServerFailure
		err = errors.New("upstream DNS response requires EDNS")
	}
	decision := "allow"
	if errors.Is(err, vmnet.ErrDenied) {
		decision = "deny"
	} else if err != nil {
		decision = "error"
	}
	s.n.logger.Info("dns query", "guest", guest, "src", src, "name", name,
		"type", dns.Type(qtype).String(), "decision", decision, "upstream", s.n.dnsUpstream,
		"rcode", dns.RcodeToString[m.Rcode], "duration", time.Since(started), "err", err)
	size := dns.MinMsgSize
	if o := r.IsEdns0(); o != nil {
		// Keep UDP replies within one frame so their complete contents
		// are queued directly to the owning port. Larger replies use TCP.
		limit := min(dnsEDNSSize, s.n.mtu-header.IPv4MinimumSize-header.UDPMinimumSize)
		m.SetEdns0(uint16(limit), false)
		if int(o.UDPSize()) > size {
			size = min(int(o.UDPSize()), limit)
		}
	}
	if w.RemoteAddr().Network() == "udp" {
		m.Truncate(size)
	}
	_ = w.WriteMsg(m)
}

func (s *dnsServer) answer(ctx context.Context, m *dns.Msg, q dns.Question, f vmnet.Flow) error {
	if q.Qclass != dns.ClassINET {
		return vmnet.ErrDenied
	}
	hdr := dns.RR_Header{Name: q.Name, Rrtype: q.Qtype, Class: dns.ClassINET, Ttl: dnsTTL}
	if q.Qtype == dns.TypePTR {
		addr, ok := reverseName(q.Name)
		if ok && s.n.subnet.Contains(addr) {
			// Guest and gateway addresses have no names to give out.
			m.Rcode = dns.RcodeNameError
			return nil
		}
		if ok && s.n.fakeIPs.contains(addr) {
			if name, ok := s.n.fakeIPs.name(addr); ok {
				m.Answer = append(m.Answer, &dns.PTR{Hdr: hdr, Ptr: dns.Fqdn(name)})
			} else {
				m.Rcode = dns.RcodeNameError
			}
			return nil
		}
	}
	authorizer, ok := s.n.egress.(vmnet.DNSAuthorizer)
	if !ok {
		return fmt.Errorf("egress does not authorize DNS: %w", vmnet.ErrDenied)
	}
	if err := authorizer.AuthorizeDNS(ctx, f, q.Qtype); err != nil {
		return err
	}
	if q.Qtype == dns.TypeAAAA {
		return nil // The guest segment carries IPv4 only.
	}
	select {
	case s.inflight <- struct{}{}:
		defer func() { <-s.inflight }()
	default:
		return errors.New("too many DNS queries in flight")
	}
	request := new(dns.Msg)
	request.SetQuestion(q.Name, q.Qtype)
	resolver := s.n.resolver.WithLogger(s.n.logger.With("guest", f.Guest, "src", f.Src))
	reply, err := resolver.Exchange(ctx, request)
	if err != nil {
		return err
	}
	if q.Qtype == dns.TypeA && reply.Rcode == dns.RcodeSuccess {
		hasAddress, hasAlias := false, false
		for _, rr := range reply.Answer {
			switch rr.(type) {
			case *dns.A:
				hasAddress = true
			case *dns.CNAME:
				hasAlias = true
			}
		}
		if !hasAddress && hasAlias {
			// A recursive upstream normally includes the terminal address.
			// Follow a partial answer while keeping the original flow name.
			addrs, err := resolver.LookupNetIP(ctx, "ip4", q.Name)
			if err != nil {
				var de *net.DNSError
				if errors.As(err, &de) && de.IsNotFound {
					m.Rcode = dns.RcodeNameError
					return nil
				}
				return err
			}
			hasAddress = len(addrs) != 0
		}
		if hasAddress {
			a, ok := s.n.fakeIPs.addr(q.Name)
			if !ok {
				return errors.New("no synthetic DNS address available")
			}
			// This is a local answer: omit upstream addresses and signatures.
			m.Answer = []dns.RR{&dns.A{Hdr: hdr, A: a.AsSlice()}}
			return nil
		}
	}
	id := m.Id
	*m = *reply
	m.Id = id
	return nil
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
