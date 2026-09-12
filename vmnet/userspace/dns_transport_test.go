package userspace

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/shazow/virtle/vm"
	"github.com/shazow/virtle/vmnet"
	"github.com/shazow/virtle/vmnet/egress"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

func TestDNSRequestsKeepTheirGuest(t *testing.T) {
	for _, transport := range []string{"udp", "tcp"} {
		t.Run(transport, func(t *testing.T) {
			var forwarded atomic.Int64
			upstream := dnsUpstream(t, func(w dns.ResponseWriter, r *dns.Msg) {
				forwarded.Add(1)
				addressDNS(w, r)
			})
			n := newTestNetwork(t, Config{Egress: &egress.Policy{Reach: egress.ReachAll}, DNSUpstream: upstream})
			first := attachGuest(t, n, "denied", vmnet.AttachOptions{Egress: &vm.Egress{}})
			ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
			defer cancel()

			// Complete a local request before installing the scheduling
			// barrier, so the DNS servers have finished their startup reads.
			local, _ := dns.ReverseAddr(n.Gateway().String())
			queryDNS(t, first, transport, local, dns.TypePTR)
			paused, release, handled := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			original := dns.Handler(n.dns)
			barrier := dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
				if r.Question[0].Name == "queued.test." {
					close(paused)
					<-release
					defer close(handled)
				}
				original.ServeDNS(w, r)
			})
			if transport == "udp" {
				n.dns.udp.Handler = barrier
			} else {
				n.dns.tcp.Handler = barrier
			}
			var conn net.Conn
			var err error
			dst := netip.AddrPortFrom(n.Gateway(), dnsPort)
			if transport == "udp" {
				conn, err = first.dialUDP(dst)
			} else {
				conn, err = first.dialTCP(ctx, dst)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			msg := new(dns.Msg).SetQuestion("queued.test.", dns.TypeTXT)
			if err := (&dns.Conn{Conn: conn}).WriteMsg(msg); err != nil {
				t.Fatal(err)
			}
			select {
			case <-paused:
			case <-ctx.Done():
				t.Fatal("query did not reach handler barrier")
			}
			if err := first.port.Close(); err != nil {
				t.Fatal(err)
			}
			second := attachGuest(t, n, "allowed", vmnet.AttachOptions{Addr: first.addr})
			if second.addr != first.addr {
				t.Fatal("address did not get reused")
			}
			releaseOnce.Do(func() { close(release) })
			select {
			case <-handled:
			case <-ctx.Done():
				t.Fatal("queued request was not handled")
			}
			if got := forwarded.Load(); got != 0 {
				t.Fatalf("old guest's query reached upstream %d times", got)
			}
			if r := queryDNS(t, second, transport, "current.test", dns.TypeA); len(r.Answer) != 1 || forwarded.Load() != 1 {
				t.Fatalf("replacement's own query failed: %v (upstream requests %d)", r, forwarded.Load())
			}
		})
	}
}

func TestDNSDatagramsKeepTheirAttachment(t *testing.T) {
	n := newTestNetwork(t, Config{Egress: vmnet.DenyAll{}})
	first := attachGuest(t, n, "first", vmnet.AttachOptions{})
	// A separately bound gateway socket gives the test control over when
	// queued datagrams are read, while using the ordinary guest packet path.
	var wq waiter.Queue
	ep, err := n.stack.NewEndpoint(udp.ProtocolNumber, ipv4.ProtocolNumber, &wq)
	if err != nil {
		t.Fatal(err)
	}
	defer ep.Close()
	const port = 1053
	if err := ep.Bind(tcpip.FullAddress{NIC: nicID, Addr: n.gateway4, Port: port}); err != nil {
		t.Fatal(err)
	}
	wrapped := &dnsUDPEndpoint{Endpoint: ep}
	pc := &dnsPacketConn{UDPConn: gonet.NewUDPConn(&wq, wrapped), n: n, ep: wrapped}
	defer pc.Close()
	_ = pc.SetReadDeadline(time.Now().Add(testTimeout))
	ready, notified := waiter.NewChannelEntry(waiter.ReadableEvents)
	wq.EventRegister(&ready)
	defer wq.EventUnregister(&ready)
	old, dialErr := first.dialUDP(netip.AddrPortFrom(n.Gateway(), port))
	if dialErr != nil {
		t.Fatal(dialErr)
	}
	defer old.Close()
	if _, err := old.Write([]byte("old")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-notified:
	case <-time.After(testTimeout):
		t.Fatal("datagram did not enter the socket queue")
	}
	if err := first.port.Close(); err != nil {
		t.Fatal(err)
	}
	second := attachGuest(t, n, "second", vmnet.AttachOptions{Addr: first.addr})
	buf := make([]byte, 64)
	k, addr, readErr := pc.ReadFrom(buf)
	if readErr != nil || string(buf[:k]) != "old" {
		t.Fatalf("queued datagram = %q, %v", buf[:k], readErr)
	}
	if addr.(*dnsAddr).p != nil {
		t.Fatal("queued datagram was assigned to the replacement guest")
	}
	current, dialErr := second.dialUDP(netip.AddrPortFrom(n.Gateway(), port))
	if dialErr != nil {
		t.Fatal(dialErr)
	}
	defer current.Close()
	if _, err := current.Write([]byte("current")); err != nil {
		t.Fatal(err)
	}
	k, addr, readErr = pc.ReadFrom(buf)
	if readErr != nil || string(buf[:k]) != "current" || addr.(*dnsAddr).p != second.port {
		t.Fatalf("current datagram = %q, %v, owner %v", buf[:k], readErr, addr)
	}
}

func TestPortCloseCancelsDNS(t *testing.T) {
	for _, transport := range []string{"udp", "tcp"} {
		t.Run(transport, func(t *testing.T) {
			started, release, handled := make(chan struct{}), make(chan struct{}), make(chan struct{})
			upstream := dnsUpstream(t, func(w dns.ResponseWriter, r *dns.Msg) {
				if r.Question[0].Name == "pending.test." {
					close(started)
					<-release
				}
				addressDNS(w, r)
			})
			defer close(release)
			n := newTestNetwork(t, Config{DNSUpstream: upstream})
			g := attachGuest(t, n, "first", vmnet.AttachOptions{})
			local, _ := dns.ReverseAddr(n.Gateway().String())
			queryDNS(t, g, transport, local, dns.TypePTR)
			handler := dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
				n.dns.ServeDNS(w, r)
				if r.Question[0].Name == "pending.test." {
					close(handled)
				}
			})
			if transport == "udp" {
				n.dns.udp.Handler = handler
			} else {
				n.dns.tcp.Handler = handler
			}
			ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
			defer cancel()
			var c net.Conn
			var err error
			dst := netip.AddrPortFrom(n.Gateway(), dnsPort)
			if transport == "udp" {
				c, err = g.dialUDP(dst)
			} else {
				c, err = g.dialTCP(ctx, dst)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if err := (&dns.Conn{Conn: c}).WriteMsg(new(dns.Msg).SetQuestion("pending.test.", dns.TypeA)); err != nil {
				t.Fatal(err)
			}
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("upstream did not receive query")
			}
			if err := g.port.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-handled:
			case <-ctx.Done():
				t.Fatal("port close did not cancel the pending DNS query")
			}
			next := attachGuest(t, n, "second", vmnet.AttachOptions{Addr: g.addr})
			if r := queryDNS(t, next, transport, "current.test", dns.TypeA); len(r.Answer) != 1 {
				t.Fatalf("replacement query = %v", r)
			}
		})
	}
}

func TestPortCloseDuringDNSHandshake(t *testing.T) {
	n := newTestNetwork(t, Config{Egress: vmnet.DenyAll{}})
	var link *stalledSYNACK
	g := attachGuestLink(t, n, "first", vmnet.AttachOptions{}, func(base vmnet.Link) vmnet.Link {
		link = &stalledSYNACK{Link: base, seen: make(chan struct{}), done: make(chan struct{})}
		return link
	})
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		if c, err := g.dialTCP(ctx, netip.AddrPortFrom(n.Gateway(), dnsPort)); err == nil {
			_ = c.Close()
		}
	}()
	select {
	case <-link.seen:
	case <-ctx.Done():
		t.Fatal("DNS handshake did not start")
	}
	closed := make(chan error, 1)
	go func() { closed <- g.port.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("port close waited for the guest's handshake")
	}
	next := attachGuest(t, n, "second", vmnet.AttachOptions{Addr: g.addr})
	local, _ := dns.ReverseAddr(n.Gateway().String())
	if r := queryDNS(t, next, "tcp", local, dns.TypePTR); r.Rcode != dns.RcodeNameError {
		t.Fatalf("replacement DNS connection = %v", r)
	}
	cancel()
	<-finished
}

func TestDNSKeepsTCPConnectionsOpen(t *testing.T) {
	n := newTestNetwork(t, Config{Egress: vmnet.DenyAll{}})
	g := attachGuest(t, n, "guest", vmnet.AttachOptions{})
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()
	c, err := g.dialTCP(ctx, netip.AddrPortFrom(n.Gateway(), dnsPort))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	local, _ := dns.ReverseAddr(n.Gateway().String())
	conn := &dns.Conn{Conn: c}
	for range 130 {
		m := new(dns.Msg).SetQuestion(local, dns.TypePTR)
		r, _, err := (&dns.Client{Timeout: testTimeout}).ExchangeWithConnContext(ctx, m, conn)
		if err != nil || r.Rcode != dns.RcodeNameError {
			t.Fatalf("DNS query on reused connection = %v, %v", r, err)
		}
	}
}
