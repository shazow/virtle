package userspace

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"

	"github.com/shazow/virtle/vm"
	"github.com/shazow/virtle/vmnet"
	"github.com/shazow/virtle/vmnet/egress"
)

func TestPortCloseEndsGuestFlows(t *testing.T) {
	for _, network := range []string{"tcp", "udp"} {
		t.Run(network, func(t *testing.T) {
			upstream, server := net.Pipe()
			defer server.Close()
			e := &recordingEgress{dial: func(context.Context, vmnet.Flow) (net.Conn, error) { return upstream, nil }}
			n := newTestNetwork(t, Config{Egress: e})
			g := attachGuest(t, n, "guest", vmnet.AttachOptions{})
			ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
			defer cancel()
			dst := netip.MustParseAddrPort("203.0.113.1:9999")
			var c net.Conn
			var err error
			if network == "tcp" {
				c, err = g.dialTCP(ctx, dst)
			} else {
				c, err = g.dialUDP(dst)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if _, err := c.Write([]byte("request")); err != nil {
				t.Fatal(err)
			}
			_ = server.SetReadDeadline(time.Now().Add(testTimeout))
			buf := make([]byte, len("request"))
			if _, err := io.ReadFull(server, buf); err != nil {
				t.Fatal(err)
			}
			if err := g.port.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := server.Read(buf); !errors.Is(err, io.EOF) {
				t.Fatalf("upstream after port close = %v, want EOF", err)
			}
		})
	}
}

func TestPortCloseCancelsGuestDial(t *testing.T) {
	started := make(chan context.Context, 1)
	e := &recordingEgress{dial: func(ctx context.Context, _ vmnet.Flow) (net.Conn, error) {
		started <- ctx
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	n := newTestNetwork(t, Config{Egress: e})
	g := attachGuest(t, n, "guest", vmnet.AttachOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		if c, err := g.dialTCP(ctx, netip.MustParseAddrPort("203.0.113.1:9999")); err == nil {
			_ = c.Close()
		}
	}()
	var dialCtx context.Context
	select {
	case dialCtx = <-started:
	case <-ctx.Done():
		t.Fatal("guest dial did not start")
	}
	if err := g.port.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-dialCtx.Done():
	case <-ctx.Done():
		t.Fatal("port close did not cancel the upstream dial")
	}
	cancel()
	<-finished
}

func TestReattachedPortGetsItsOwnUDPPolicy(t *testing.T) {
	server, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	go func() {
		buf := make([]byte, 128)
		for {
			n, from, err := server.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = server.WriteTo(buf[:n], from)
		}
	}()
	policy := &egress.Policy{
		Rules:        []egress.Rule{{Hosts: []string{"example.test"}}},
		DenyPrefixes: []netip.Prefix{},
		Resolver:     hostTable{"example.test": netip.MustParseAddr("127.0.0.1")},
	}
	denied := make(chan error, 1)
	e := &recordingEgress{dial: func(ctx context.Context, f vmnet.Flow) (net.Conn, error) {
		c, err := policy.DialFlow(ctx, f)
		if f.Guest == "second" {
			denied <- err
		}
		return c, err
	}}
	n := newTestNetwork(t, Config{DNS: DNSFakeIP, Egress: e})
	first := attachGuest(t, n, "first", vmnet.AttachOptions{})
	addr, ok := n.fakeIPs.addr("example.test")
	if !ok {
		t.Fatal("synthetic address unavailable")
	}
	dst := netip.AddrPortFrom(addr, uint16(server.LocalAddr().(*net.UDPAddr).Port))
	c, err := first.dialUDP(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	echo(t, c, "first guest")
	sourcePort := uint16(c.LocalAddr().(*net.UDPAddr).Port)
	if err := first.port.Close(); err != nil {
		t.Fatal(err)
	}
	second := attachGuest(t, n, "second", vmnet.AttachOptions{Addr: first.addr, Egress: &vm.Egress{}})
	local := full(netip.AddrPortFrom(second.addr, sourcePort))
	remote := full(dst)
	u, err := gonet.DialUDP(second.stack, &local, &remote, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	_ = u.SetReadDeadline(time.Now().Add(testTimeout))
	if _, err := u.Write([]byte("second guest")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-denied:
		if !errors.Is(err, vmnet.ErrDenied) {
			t.Fatalf("second guest flow = %v, want refusal by its policy", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("reattached guest did not get a fresh egress decision")
	}
	if f := e.last(t); f.Guest != "second" || f.Egress == nil {
		t.Fatalf("new flow = %+v, want second guest's policy", f)
	}
}

// stalledSYNACK keeps the guest from completing its handshake. Closing it
// releases its writer, just as closing a blocked stream socket would.
type stalledSYNACK struct {
	vmnet.Link
	seen, done          chan struct{}
	seenOnce, closeOnce sync.Once
}

func (l *stalledSYNACK) WriteFrame(frame []byte) error {
	if len(frame) >= header.EthernetMinimumSize+header.IPv4MinimumSize+header.TCPMinimumSize && header.Ethernet(frame).Type() == header.IPv4ProtocolNumber {
		ip := header.IPv4(frame[header.EthernetMinimumSize:])
		if ip.Protocol() == uint8(header.TCPProtocolNumber) {
			tcp := header.TCP(frame[header.EthernetMinimumSize+int(ip.HeaderLength()):])
			if tcp.Flags().Contains(header.TCPFlagSyn | header.TCPFlagAck) {
				l.seenOnce.Do(func() { close(l.seen) })
				<-l.done
				return net.ErrClosed
			}
		}
	}
	return l.Link.WriteFrame(frame)
}

func (l *stalledSYNACK) Close() error {
	l.closeOnce.Do(func() { close(l.done) })
	return l.Link.Close()
}

func TestPortCloseDuringTCPHandshake(t *testing.T) {
	upstream, server := net.Pipe()
	defer server.Close()
	e := &recordingEgress{dial: func(context.Context, vmnet.Flow) (net.Conn, error) { return upstream, nil }}
	n := newTestNetwork(t, Config{Egress: e})
	var link *stalledSYNACK
	g := attachGuestLink(t, n, "guest", vmnet.AttachOptions{}, func(base vmnet.Link) vmnet.Link {
		link = &stalledSYNACK{Link: base, seen: make(chan struct{}), done: make(chan struct{})}
		return link
	})
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		if c, err := g.dialTCP(ctx, netip.MustParseAddrPort("203.0.113.1:9999")); err == nil {
			_ = c.Close()
		}
	}()
	select {
	case <-link.seen:
	case <-ctx.Done():
		t.Fatal("guest handshake did not start")
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
	_ = server.SetReadDeadline(time.Now().Add(testTimeout))
	if _, err := server.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("upstream after port close = %v, want EOF", err)
	}
	p, err := n.Attach(ctx, idleLink(t, n.MTU()), vmnet.AttachOptions{Addr: g.addr})
	if err != nil {
		t.Fatalf("reuse address after canceled handshake: %v", err)
	}
	_ = p.Close()
	cancel()
	<-finished
}

// gatedCloseConn makes upstream teardown observable without a timed sleep.
type gatedCloseConn struct {
	net.Conn
	entered, release chan struct{}
	once             sync.Once
}

func (c *gatedCloseConn) Close() error {
	c.once.Do(func() { close(c.entered) })
	<-c.release
	return c.Conn.Close()
}

func TestPortCloseJoinsFlowCleanup(t *testing.T) {
	upstream, server := net.Pipe()
	defer server.Close()
	gate := &gatedCloseConn{Conn: upstream, entered: make(chan struct{}), release: make(chan struct{})}
	var release sync.Once
	defer release.Do(func() { close(gate.release) })
	e := &recordingEgress{dial: func(context.Context, vmnet.Flow) (net.Conn, error) { return gate, nil }}
	n := newTestNetwork(t, Config{Egress: e})
	pp, err := n.Attach(context.Background(), idleLink(t, n.MTU()), vmnet.AttachOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p := pp.(*port)
	id := stack.TransportEndpointID{RemoteAddress: p.addr4, RemotePort: 1234, LocalAddress: addr4(netip.MustParseAddr("203.0.113.1")), LocalPort: 80}
	f, err := p.dialFlow(vm.TCP, id, dialTimeout)
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan struct{})
	go func() { f.finish(); close(finished) }()
	<-gate.entered
	closed := make(chan error, 1)
	go func() { closed <- p.Close() }()
	<-p.ctx.Done() // Port teardown has started while flow cleanup is blocked.
	if replacement, err := n.Attach(context.Background(), idleLink(t, n.MTU()), vmnet.AttachOptions{Addr: p.addr}); err == nil {
		_ = replacement.Close()
		t.Error("port released its address before upstream cleanup completed")
	}
	release.Do(func() { close(gate.release) })
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	<-finished
	if replacement, err := n.Attach(context.Background(), idleLink(t, n.MTU()), vmnet.AttachOptions{Addr: p.addr}); err != nil {
		t.Fatalf("address unavailable after cleanup: %v", err)
	} else {
		_ = replacement.Close()
	}
}

// A queued SYN belongs to its attachment, even when another guest reuses
// the address and the entire TCP tuple before the handler gets to run.
func TestReattachedPortGetsItsOwnTCPPolicy(t *testing.T) {
	upstream, server := net.Pipe()
	defer server.Close()
	go func() { _, _ = io.Copy(server, server) }()
	e := &recordingEgress{dial: func(context.Context, vmnet.Flow) (net.Conn, error) { return upstream, nil }}
	n := newTestNetwork(t, Config{Egress: e})
	first := attachGuest(t, n, "first", vmnet.AttachOptions{})
	p := first.port.(*port)
	queued := make(chan stack.TransportEndpointID, 1)
	resume, handled := make(chan struct{}), make(chan struct{})
	var release sync.Once
	defer release.Do(func() { close(resume) })
	p.tcpForwarder = tcp.NewForwarder(n.stack, 0, maxInFlight, func(r *tcp.ForwarderRequest) {
		queued <- r.ID()
		<-resume
		p.handleTCP(r)
		close(handled)
	})
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()
	firstCtx, cancelFirst := context.WithCancel(ctx)
	defer cancelFirst()
	firstDone := make(chan struct{})
	dst := netip.MustParseAddrPort("203.0.113.1:9999")
	go func() {
		defer close(firstDone)
		if c, err := first.dialTCP(firstCtx, dst); err == nil {
			_ = c.Close()
		}
	}()
	var id stack.TransportEndpointID
	select {
	case id = <-queued:
	case <-ctx.Done():
		t.Fatal("first SYN was not dispatched")
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	cancelFirst()
	<-firstDone
	policy := &vm.Egress{}
	second := attachGuest(t, n, "second", vmnet.AttachOptions{Addr: first.addr, Egress: policy})
	local := full(netip.AddrPortFrom(second.addr, id.RemotePort))
	c, err := gonet.DialTCPWithBind(ctx, second.stack, local, full(dst), ipv4.ProtocolNumber)
	if err != nil {
		t.Fatalf("replacement dial with the original tuple: %v", err)
	}
	defer c.Close()
	echo(t, c, "replacement before original handler resumes")
	resets := n.stack.Stats().TCP.ResetsSent.Value()
	release.Do(func() { close(resume) })
	select {
	case <-handled:
	case <-ctx.Done():
		t.Fatal("original handler did not finish")
	}
	if got := n.stack.Stats().TCP.ResetsSent.Value(); got != resets {
		t.Errorf("original handler sent %d resets after address reuse", got-resets)
	}
	echo(t, c, "replacement after original handler finishes")
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.flows) != 1 || e.flows[0].Guest != "second" || e.flows[0].Egress != policy {
		t.Fatalf("egress flows = %+v, want only replacement guest's policy", e.flows)
	}
}
