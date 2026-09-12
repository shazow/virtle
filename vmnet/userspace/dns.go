package userspace

import (
	"net/netip"
	"strings"

	"github.com/miekg/dns"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
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
	s := &dnsServer{n: n, udpConn: gonet.NewUDPConn(&wq, ep)}
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
		m.Rcode = dns.RcodeRefused
		return
	}
	hdr := dns.RR_Header{Name: q.Name, Rrtype: q.Qtype, Class: dns.ClassINET, Ttl: dnsTTL}
	switch q.Qtype {
	case dns.TypeA:
		// Resolving the real name belongs to the Egress, after its policy
		// admits the flow. DNS itself must never contact an upstream.
		a, ok := s.n.fakeIPs.addr(q.Name)
		if !ok {
			m.Rcode = dns.RcodeServerFailure
			return
		}
		m.Answer = append(m.Answer, &dns.A{Hdr: hdr, A: a.AsSlice()})
	case dns.TypeAAAA:
		// The segment carries IPv4 only; an empty answer lets the client
		// use A without claiming the name does not exist.
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
		if s.n.fakeIPs.contains(addr) {
			if name, ok := s.n.fakeIPs.name(addr); ok {
				m.Answer = append(m.Answer, &dns.PTR{Hdr: hdr, Ptr: dns.Fqdn(name)})
			} else {
				m.Rcode = dns.RcodeNameError
			}
			return
		}
		m.Rcode = dns.RcodeRefused
	default:
		m.Rcode = dns.RcodeRefused
	}
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
