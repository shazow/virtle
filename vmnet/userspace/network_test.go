package userspace

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/miekg/dns"

	"github.com/shazow/virtle/vm"
	"github.com/shazow/virtle/vmnet"
)

// recordingEgress remembers every flow and dials with dial.
type recordingEgress struct {
	dial func(ctx context.Context, f vmnet.Flow) (net.Conn, error)

	mu    sync.Mutex
	flows []vmnet.Flow
}

func (e *recordingEgress) DialFlow(ctx context.Context, f vmnet.Flow) (net.Conn, error) {
	e.mu.Lock()
	e.flows = append(e.flows, f)
	e.mu.Unlock()
	return e.dial(ctx, f)
}

func (e *recordingEgress) last(t *testing.T) vmnet.Flow {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.flows) == 0 {
		t.Fatal("no flow reached the egress")
	}
	return e.flows[len(e.flows)-1]
}

func TestDHCPLeaseMatchesPort(t *testing.T) {
	n := newTestNetwork(t, Config{})
	g := attachGuest(t, n, "vm1", vmnet.AttachOptions{})
	if g.addr != g.port.Addr() {
		t.Fatalf("leased %s, port reports %s", g.addr, g.port.Addr())
	}
	if !n.Subnet().Contains(g.addr) || g.addr == n.Gateway() {
		t.Fatalf("leased %s outside %s", g.addr, n.Subnet())
	}
	ack := g.ack
	if got := ack.Router(); len(got) != 1 || !got[0].Equal(n.Gateway().AsSlice()) {
		t.Errorf("router = %v, want %s", got, n.Gateway())
	}
	if got := ack.DNS(); len(got) != 1 || !got[0].Equal(n.Gateway().AsSlice()) {
		t.Errorf("dns = %v, want %s", got, n.Gateway())
	}
	if ones, _ := ack.SubnetMask().Size(); ones != n.Subnet().Bits() {
		t.Errorf("mask = /%d, want /%d", ones, n.Subnet().Bits())
	}
	if mtu, err := dhcpv4.GetUint16(dhcpv4.OptionInterfaceMTU, ack.Options); err != nil || int(mtu) != n.MTU() {
		t.Errorf("mtu option = %d, %v; want %d", mtu, err, n.MTU())
	}
	if ack.HostName() != "vm1" {
		t.Errorf("hostname = %q, want vm1", ack.HostName())
	}
	if ack.IPAddressLeaseTime(0) != leaseTime {
		t.Errorf("lease time = %s, want %s", ack.IPAddressLeaseTime(0), leaseTime)
	}
	if !ack.ServerIdentifier().Equal(n.Gateway().AsSlice()) {
		t.Errorf("server identifier = %s", ack.ServerIdentifier())
	}
	if want := macFor(g.addr); g.port.MAC().String() != want.String() {
		t.Errorf("MAC = %s, want %s derived from the address", g.port.MAC(), want)
	}
}

func TestGuestFlowsGoThroughEgress(t *testing.T) {
	tcpEcho, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcpEcho.Close()
	go serveEcho(tcpEcho)
	udpEcho, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer udpEcho.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			k, from, err := udpEcho.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = udpEcho.WriteTo(buf[:k], from)
		}
	}()
	// The guest addresses a public destination; the egress redirects it to
	// the local echo, as a policy or proxy would.
	egress := &recordingEgress{dial: func(ctx context.Context, f vmnet.Flow) (net.Conn, error) {
		var d net.Dialer
		if f.Proto == vm.UDP {
			return d.DialContext(ctx, "udp", udpEcho.LocalAddr().String())
		}
		return d.DialContext(ctx, "tcp", tcpEcho.Addr().String())
	}}
	n := newTestNetwork(t, Config{Egress: egress})
	g := attachGuest(t, n, "vm1", vmnet.AttachOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	dst := netip.MustParseAddrPort("203.0.113.10:80")
	c, err := g.dialTCP(ctx, dst)
	if err != nil {
		t.Fatalf("guest dial: %v", err)
	}
	echo(t, c, "over tcp")
	c.Close()
	f := egress.last(t)
	if f.Proto != vm.TCP || f.Dst != dst || f.Src.Addr() != g.addr || f.Guest != "vm1" {
		t.Errorf("flow = %+v, want tcp %s from %s by vm1", f, dst, g.addr)
	}

	dst = netip.MustParseAddrPort("203.0.113.10:5353")
	u, err := g.dialUDP(dst)
	if err != nil {
		t.Fatalf("guest dial udp: %v", err)
	}
	echo(t, u, "over udp")
	u.Close()
	if f := egress.last(t); f.Proto != vm.UDP || f.Dst != dst || f.Guest != "vm1" {
		t.Errorf("udp flow = %+v, want udp %s by vm1", f, dst)
	}
}

func TestDeniedFlowIsRefusedBeforeAccept(t *testing.T) {
	egress := &recordingEgress{dial: vmnet.DenyAll{}.DialFlow}
	n := newTestNetwork(t, Config{Egress: egress})
	g := attachGuest(t, n, "vm1", vmnet.AttachOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	start := time.Now()
	if c, err := g.dialTCP(ctx, netip.MustParseAddrPort("203.0.113.10:80")); err == nil {
		c.Close()
		t.Fatal("a denied TCP flow connected")
	} else if !strings.Contains(err.Error(), "refused") {
		t.Fatalf("denied TCP flow failed with %v, want a refusal", err)
	}
	if time.Since(start) > testTimeout/2 {
		t.Fatalf("the refusal took %s; the guest should see a reset, not a timeout", time.Since(start))
	}
	u, err := g.dialUDP(netip.MustParseAddrPort("203.0.113.10:53"))
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	if _, err := u.Write([]byte("?")); err != nil {
		t.Fatal(err)
	}
	// The guest stack records the port unreachable as the socket's pending
	// error; gonet reads only wake for data, so poll for it.
	deadline := time.Now().Add(testTimeout)
	for {
		_ = u.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		_, err = u.Read(make([]byte, 16))
		var ne net.Error
		if !(errors.As(err, &ne) && ne.Timeout()) || time.Now().After(deadline) {
			break
		}
	}
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("denied UDP flow read = %v, want a port unreachable", err)
	}
	if got := len(egress.flows); got != 2 {
		t.Errorf("egress saw %d flows, want 2", got)
	}
}

func TestExposeReachesGuestListener(t *testing.T) {
	n := newTestNetwork(t, Config{})
	g := attachGuest(t, n, "vm1", vmnet.AttachOptions{})
	g.listenTCP(7)
	g.listenUDP(9)
	ctx := context.Background()

	host := freePort(t, "tcp")
	closer, err := g.port.Expose(ctx, vm.Forward{HostAddr: host, GuestAddr: ":7"})
	if err != nil {
		t.Fatalf("Expose: %v", err)
	}
	c, err := net.DialTimeout("tcp", host, testTimeout)
	if err != nil {
		t.Fatalf("dial the forward: %v", err)
	}
	echo(t, c, "through the forward")
	c.Close()
	if _, err := g.port.Expose(ctx, vm.Forward{HostAddr: host, GuestAddr: ":7"}); err == nil {
		t.Fatal("a duplicate Expose succeeded")
	}
	if _, err := g.port.Expose(ctx, vm.Forward{HostAddr: freePort(t, "tcp"), GuestAddr: "192.0.2.1:7"}); err == nil {
		t.Fatal("Expose accepted a guest address that is not this port's")
	}
	if _, err := g.port.Expose(ctx, vm.Forward{HostAddr: freePort(t, "tcp"), GuestAddr: g.addr.String() + ":7"}); err != nil {
		t.Fatalf("Expose with the port's own address: %v", err)
	}
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	if c, err := net.DialTimeout("tcp", host, time.Second); err == nil {
		c.Close()
		t.Fatal("the closed forward still accepts")
	}

	hostUDP := freePort(t, "udp")
	if _, err := g.port.Expose(ctx, vm.Forward{HostAddr: hostUDP, GuestAddr: ":9", Proto: vm.UDP}); err != nil {
		t.Fatalf("Expose udp: %v", err)
	}
	u, err := net.Dial("udp", hostUDP)
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	echo(t, u, "datagram through the forward")

	_ = g.port.Close()
	if _, err := g.port.Expose(ctx, vm.Forward{HostAddr: freePort(t, "tcp"), GuestAddr: ":7"}); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Expose after Close = %v, want net.ErrClosed", err)
	}
}

func TestHostDialsAndServesGuests(t *testing.T) {
	n := newTestNetwork(t, Config{})
	g := attachGuest(t, n, "vm1", vmnet.AttachOptions{})
	g.listenTCP(7)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	for _, addr := range []string{g.addr.String() + ":7", "vm1:7"} {
		c, err := n.DialContext(ctx, "tcp", addr)
		if err != nil {
			t.Fatalf("DialContext %s: %v", addr, err)
		}
		echo(t, c, "from the host to "+addr)
		c.Close()
	}
	if _, err := n.DialContext(ctx, "tcp", "vm2:7"); err == nil {
		t.Fatal("DialContext resolved a machine that is not attached")
	}
	if _, err := n.DialContext(ctx, "tcp", "10.1.1.1:7"); err == nil {
		t.Fatal("DialContext accepted an address outside the network")
	}

	ln, err := n.Listen("tcp", ":8080")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()
	go serveEcho(ln)
	c, err := g.dialTCP(ctx, netip.AddrPortFrom(n.Gateway(), 8080))
	if err != nil {
		t.Fatalf("guest dial the gateway service: %v", err)
	}
	echo(t, c, "to a host service")
	c.Close()
	if _, err := n.Listen("tcp", g.addr.String()+":1"); err == nil {
		t.Fatal("Listen bound an address that is not the gateway's")
	}
}

func TestGuestsReachEachOther(t *testing.T) {
	egress := &recordingEgress{dial: vmnet.DenyAll{}.DialFlow}
	n := newTestNetwork(t, Config{Egress: egress})
	g1 := attachGuest(t, n, "vm1", vmnet.AttachOptions{})
	g2 := attachGuest(t, n, "vm2", vmnet.AttachOptions{})
	if g1.addr == g2.addr || g1.mac == g2.mac {
		t.Fatalf("guests share an identity: %s/%s and %s/%s", g1.addr, g1.mac, g2.addr, g2.mac)
	}
	g2.listenTCP(7)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	c, err := g1.dialTCP(ctx, netip.AddrPortFrom(g2.addr, 7))
	if err != nil {
		t.Fatalf("vm1 dial vm2: %v", err)
	}
	echo(t, c, "neighbor")
	c.Close()
	if len(egress.flows) != 0 {
		t.Errorf("guest-to-guest traffic reached the egress: %+v", egress.flows)
	}
}

func TestAttachOptions(t *testing.T) {
	n := newTestNetwork(t, Config{})
	fixed := vmnet.AttachOptions{
		Addr: netip.MustParseAddr("192.168.127.50"),
		MAC:  net.HardwareAddr{0x02, 0x11, 0x22, 0x33, 0x44, 0x55},
	}
	g := attachGuest(t, n, "resumed", fixed)
	if g.addr != fixed.Addr || g.port.MAC().String() != fixed.MAC.String() {
		t.Fatalf("port = %s/%s, want the fixed %s/%s", g.addr, g.port.MAC(), fixed.Addr, fixed.MAC)
	}
	for name, opts := range map[string]vmnet.AttachOptions{
		"address in use":     {Addr: fixed.Addr},
		"MAC in use":         {MAC: fixed.MAC},
		"gateway address":    {Addr: n.Gateway()},
		"outside the subnet": {Addr: netip.MustParseAddr("10.0.0.2")},
		"multicast MAC":      {MAC: net.HardwareAddr{0x01, 0, 0, 0, 0, 1}},
		"gateway MAC":        {MAC: macFor(n.Gateway())},
	} {
		hostEnd, guestEnd := net.Pipe()
		defer guestEnd.Close()
		if p, err := n.Attach(context.Background(), vmnet.Tunnel(hostEnd, n.MTU()), opts); err == nil {
			p.Close()
			t.Errorf("Attach with %s succeeded", name)
		}
	}
	small, other := net.Pipe()
	defer other.Close()
	if p, err := n.Attach(context.Background(), vmnet.Tunnel(small, n.MTU()-1), vmnet.AttachOptions{}); err == nil {
		p.Close()
		t.Fatal("Attach accepted a link with a smaller MTU")
	}
}

func TestAddressesRunOut(t *testing.T) {
	n := newTestNetwork(t, Config{Subnet: netip.MustParsePrefix("10.9.0.0/29")})
	var ports []vmnet.Port
	for i := 0; i < 5; i++ {
		hostEnd, guestEnd := net.Pipe()
		defer guestEnd.Close()
		p, err := n.Attach(context.Background(), vmnet.Tunnel(hostEnd, n.MTU()), vmnet.AttachOptions{})
		if err != nil {
			t.Fatalf("attach %d: %v", i, err)
		}
		ports = append(ports, p)
	}
	hostEnd, guestEnd := net.Pipe()
	defer guestEnd.Close()
	if p, err := n.Attach(context.Background(), vmnet.Tunnel(hostEnd, n.MTU()), vmnet.AttachOptions{}); err == nil {
		p.Close()
		t.Fatal("a /29 attached a sixth guest")
	}
	first := ports[0].Addr()
	_ = ports[0].Close()
	p, err := n.Attach(context.Background(), vmnet.NewDeferred(n.MTU()), vmnet.AttachOptions{})
	if err != nil {
		t.Fatalf("attach after a release: %v", err)
	}
	if p.Addr() != first {
		t.Fatalf("reused %s, want the released %s", p.Addr(), first)
	}
}

// fakeResolver answers from a table; anything else is a not-found error.
type fakeResolver struct {
	*net.Resolver
	a map[string]netip.Addr
}

func (r fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if a, ok := r.a[host]; ok {
		return []netip.Addr{a}, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

func TestDNSForwardsToTheHostResolver(t *testing.T) {
	n := newTestNetwork(t, Config{})
	n.dns.resolver = fakeResolver{a: map[string]netip.Addr{"example.test.": netip.MustParseAddr("192.0.2.7")}}
	g := attachGuest(t, n, "vm1", vmnet.AttachOptions{})
	resolver := netip.AddrPortFrom(n.Gateway(), dnsPort)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	ask := func(c net.Conn, name string, qtype uint16) *dns.Msg {
		t.Helper()
		m := new(dns.Msg)
		m.SetQuestion(name, qtype)
		m.SetEdns0(4096, false)
		r, _, err := (&dns.Client{Timeout: testTimeout}).ExchangeWithConnContext(ctx, m, &dns.Conn{Conn: c})
		if err != nil {
			t.Fatalf("query %s %s: %v", name, dns.TypeToString[qtype], err)
		}
		return r
	}
	u, err := g.dialUDP(resolver)
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	r := ask(u, "example.test.", dns.TypeA)
	if r.Rcode != dns.RcodeSuccess || len(r.Answer) != 1 {
		t.Fatalf("A answer = %v", r)
	}
	if a, ok := r.Answer[0].(*dns.A); !ok || a.A.String() != "192.0.2.7" {
		t.Fatalf("A record = %v", r.Answer[0])
	}
	if r := ask(u, "example.test.", dns.TypeAAAA); r.Rcode != dns.RcodeSuccess || len(r.Answer) != 0 {
		t.Fatalf("AAAA answer = %v, want an empty success", r)
	}
	if r := ask(u, "nope.test.", dns.TypeA); r.Rcode != dns.RcodeNameError {
		t.Fatalf("unknown name rcode = %s, want NXDOMAIN", dns.RcodeToString[r.Rcode])
	}
	if r := ask(u, "7.2.0.192.in-addr.arpa.", dns.TypePTR); r.Rcode == dns.RcodeNotImplemented {
		t.Fatalf("PTR is not served")
	}
	if r := ask(u, "2.127.168.192.in-addr.arpa.", dns.TypePTR); r.Rcode != dns.RcodeNameError {
		t.Fatalf("PTR for a guest address = %s, want NXDOMAIN", dns.RcodeToString[r.Rcode])
	}
	tc, err := g.dialTCP(ctx, resolver)
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	if r := ask(tc, "example.test.", dns.TypeA); len(r.Answer) != 1 {
		t.Fatalf("A over TCP = %v", r)
	}
}

func TestCloseEndsEverything(t *testing.T) {
	n := newTestNetwork(t, Config{})
	g := attachGuest(t, n, "vm1", vmnet.AttachOptions{})
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := g.port.Expose(context.Background(), vm.Forward{HostAddr: "127.0.0.1:0", GuestAddr: ":7"}); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Expose after network Close = %v, want net.ErrClosed", err)
	}
	if _, err := n.Attach(context.Background(), vmnet.NewDeferred(n.MTU()), vmnet.AttachOptions{}); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Attach after Close = %v, want net.ErrClosed", err)
	}
	if _, ok := g.readFrame(time.Second); ok {
		t.Fatal("the guest link is still open")
	}
	if err := n.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestConfigValidation(t *testing.T) {
	for name, cfg := range map[string]Config{
		"ipv6 subnet":          {Subnet: netip.MustParsePrefix("fd00::/64")},
		"subnet too small":     {Subnet: netip.MustParsePrefix("10.0.0.0/31")},
		"gateway outside":      {Gateway: netip.MustParseAddr("10.0.0.1")},
		"gateway is network":   {Gateway: netip.MustParseAddr("192.168.127.0")},
		"gateway is broadcast": {Gateway: netip.MustParseAddr("192.168.127.255")},
		"mtu too small":        {MTU: 100},
		"unknown dns mode":     {DNS: "magic"},
	} {
		if n, err := New(cfg); err == nil {
			n.Close()
			t.Errorf("New accepted %s", name)
		}
	}
	n := newTestNetwork(t, Config{Subnet: netip.MustParsePrefix("10.20.30.64/26"), MTU: 9000})
	if n.Gateway() != netip.MustParseAddr("10.20.30.65") || n.MTU() != 9000 {
		t.Fatalf("gateway %s mtu %d", n.Gateway(), n.MTU())
	}
	p, err := n.Attach(context.Background(), vmnet.NewDeferred(9000), vmnet.AttachOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Addr() != netip.MustParseAddr("10.20.30.66") {
		t.Fatalf("first guest at %s", p.Addr())
	}
}

func TestGatewayPortsAreClosedNotForwarded(t *testing.T) {
	egress := &recordingEgress{dial: vmnet.Passthrough{}.DialFlow}
	n := newTestNetwork(t, Config{Egress: egress})
	g := attachGuest(t, n, "vm1", vmnet.AttachOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	start := time.Now()
	if c, err := g.dialTCP(ctx, netip.AddrPortFrom(n.Gateway(), 9)); err == nil {
		c.Close()
		t.Fatal("a gateway port with nothing behind it accepted")
	} else if !strings.Contains(err.Error(), "refused") || time.Since(start) > testTimeout/2 {
		t.Fatalf("dialing the gateway failed with %v after %s, want a prompt refusal", err, time.Since(start))
	}
	egress.mu.Lock()
	defer egress.mu.Unlock()
	if len(egress.flows) != 0 {
		t.Errorf("the egress saw %d flows to the gateway, want none: it is not a way out", len(egress.flows))
	}
}

func TestExposeRacesWithClose(t *testing.T) {
	n := newTestNetwork(t, Config{})
	for i := 0; i < 20; i++ {
		hostEnd, guestEnd := net.Pipe()
		p, err := n.Attach(context.Background(), vmnet.QEMUStream(hostEnd, n.MTU()), vmnet.AttachOptions{})
		if err != nil {
			t.Fatal(err)
		}
		host := freePort(t, "tcp")
		exposed := make(chan error, 1)
		go func() {
			_, err := p.Expose(context.Background(), vm.Forward{HostAddr: host, GuestAddr: ":7"})
			exposed <- err
		}()
		_ = p.Close()
		if err := <-exposed; err == nil {
			// Exposed before the close, so the close took the listener.
			if c, err := net.DialTimeout("tcp", host, time.Second); err == nil {
				c.Close()
				t.Fatal("a forward exposed on a port that closed still listens")
			}
		} else if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Expose during Close = %v", err)
		}
		guestEnd.Close()
	}
}
