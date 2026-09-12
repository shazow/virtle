package userspace

import (
	"context"
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
)

// unansweredSYN drops connection attempts to one guest port, reporting their
// endpoint ID so the test can observe handshake cleanup without retransmission
// timers or a guest response.
type unansweredSYN struct {
	vmnet.Link
	port uint16
	seen chan stack.TransportEndpointID
}

func (l *unansweredSYN) WriteFrame(frame []byte) error {
	const l3 = header.EthernetMinimumSize
	if len(frame) >= l3+header.IPv4MinimumSize+header.TCPMinimumSize && header.Ethernet(frame).Type() == header.IPv4ProtocolNumber {
		ip := header.IPv4(frame[l3:])
		if ip.Protocol() == uint8(tcp.ProtocolNumber) {
			h := header.TCP(frame[l3+int(ip.HeaderLength()):])
			if h.DestinationPort() == l.port && h.Flags() == header.TCPFlagSyn {
				select {
				case l.seen <- stack.TransportEndpointID{
					LocalAddress: ip.SourceAddress(), LocalPort: h.SourcePort(),
					RemoteAddress: ip.DestinationAddress(), RemotePort: h.DestinationPort(),
				}:
				default:
				}
				return nil
			}
		}
	}
	return l.Link.WriteFrame(frame)
}

func TestExposureCloseDuringTCPHandshake(t *testing.T) {
	for _, closePort := range []bool{false, true} {
		name := "exposure"
		if closePort {
			name = "port"
		}
		t.Run(name, func(t *testing.T) {
			n := newTestNetwork(t, Config{})
			seen := make(chan stack.TransportEndpointID, 1)
			g := attachGuestLink(t, n, "first", vmnet.AttachOptions{}, func(link vmnet.Link) vmnet.Link {
				return &unansweredSYN{Link: link, port: 8, seen: seen}
			})
			exposed, err := g.port.Expose(t.Context(), vm.Forward{HostAddr: "127.0.0.1:0", GuestAddr: ":8"})
			if err != nil {
				t.Fatal(err)
			}
			e := exposed.(*exposure)
			host := e.closer.(net.Listener).Addr().String()
			c, err := net.DialTimeout("tcp", host, testTimeout)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
			defer cancel()
			var id stack.TransportEndpointID
			select {
			case id = <-seen:
			case <-ctx.Done():
				t.Fatal("forward did not start the guest handshake")
			}
			if ep := n.stack.FindTransportEndpoint(ipv4.ProtocolNumber, tcp.ProtocolNumber, id, nicID); ep == nil {
				t.Fatal("guest handshake has no transport endpoint")
			}
			closed := make(chan error, 1)
			go func() {
				if closePort {
					closed <- g.port.Close()
				} else {
					closed <- exposed.Close()
				}
			}()
			select {
			case err := <-closed:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("close waited for the guest handshake timeout")
			}
			if ep := n.stack.FindTransportEndpoint(ipv4.ProtocolNumber, tcp.ProtocolNumber, id, nicID); ep != nil {
				t.Fatal("close returned before releasing the pending guest handshake")
			}
			_ = c.SetReadDeadline(time.Now().Add(testTimeout))
			if _, err := c.Read(make([]byte, 1)); err != io.EOF {
				t.Fatalf("host read after close = %v, want EOF", err)
			}

			if closePort {
				g = attachGuest(t, n, "replacement", vmnet.AttachOptions{Addr: g.addr})
			}
			g.listenTCP(7)
			next, err := g.port.Expose(t.Context(), vm.Forward{HostAddr: host, GuestAddr: ":7"})
			if err != nil {
				t.Fatalf("re-expose after close: %v", err)
			}
			defer next.Close()
			c2, err := net.DialTimeout("tcp", host, testTimeout)
			if err != nil {
				t.Fatal(err)
			}
			defer c2.Close()
			echo(t, c2, "fresh exposure")
		})
	}
}

func TestExposurePreservesTCPHalfClose(t *testing.T) {
	n := newTestNetwork(t, Config{})
	g := attachGuest(t, n, "guest", vmnet.AttachOptions{})
	ln, err := gonet.ListenTCP(g.stack, full(netip.AddrPortFrom(g.addr, 7)), ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(testTimeout))
		request, err := io.ReadAll(c)
		if err == nil {
			_, err = c.Write(append([]byte("reply: "), request...))
		}
		done <- err
	}()
	exposed, err := g.port.Expose(t.Context(), vm.Forward{HostAddr: "127.0.0.1:0", GuestAddr: ":7"})
	if err != nil {
		t.Fatal(err)
	}
	defer exposed.Close()
	host := exposed.(*exposure).closer.(net.Listener).Addr().String()
	c, err := net.DialTimeout("tcp", host, testTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(testTimeout))
	if _, err := c.Write([]byte("request")); err != nil {
		t.Fatal(err)
	}
	if err := c.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	reply, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	if string(reply) != "reply: request" {
		t.Fatalf("reply = %q", reply)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// gatedExposureListener keeps a real exposure's teardown in progress so a
// concurrent port close must join it before releasing the guest's address.
type gatedExposureListener struct {
	net.Listener
	entered, release chan struct{}
	once             sync.Once
}

func (ln *gatedExposureListener) Close() error {
	ln.once.Do(func() { close(ln.entered) })
	<-ln.release
	return ln.Listener.Close()
}

func TestPortCloseJoinsExposureCleanup(t *testing.T) {
	n := newTestNetwork(t, Config{})
	g := attachGuest(t, n, "guest", vmnet.AttachOptions{})
	exposed, err := g.port.Expose(t.Context(), vm.Forward{HostAddr: "127.0.0.1:0", GuestAddr: ":7"})
	if err != nil {
		t.Fatal(err)
	}
	e := exposed.(*exposure)
	gate := &gatedExposureListener{Listener: e.closer.(net.Listener), entered: make(chan struct{}), release: make(chan struct{})}
	var release sync.Once
	defer release.Do(func() { close(gate.release) })
	e.closer = gate
	exposureClosed := make(chan error, 1)
	go func() { exposureClosed <- e.Close() }()
	<-gate.entered
	portClosed := make(chan error, 1)
	go func() { portClosed <- g.port.Close() }()
	<-e.p.ctx.Done()
	if p, err := n.Attach(t.Context(), idleLink(t, n.MTU()), vmnet.AttachOptions{Addr: g.addr}); err == nil {
		_ = p.Close()
		t.Error("port released its address before exposure cleanup completed")
	}
	release.Do(func() { close(gate.release) })
	if err := <-exposureClosed; err != nil {
		t.Fatal(err)
	}
	if err := <-portClosed; err != nil {
		t.Fatal(err)
	}
	p, err := n.Attach(t.Context(), idleLink(t, n.MTU()), vmnet.AttachOptions{Addr: g.addr})
	if err != nil {
		t.Fatalf("reuse address after exposure cleanup: %v", err)
	}
	_ = p.Close()
}

func TestExposureCloseReleasesEstablishedTCP(t *testing.T) {
	n := newTestNetwork(t, Config{})
	g := attachGuest(t, n, "first", vmnet.AttachOptions{})
	ln, err := gonet.ListenTCP(g.stack, full(netip.AddrPortFrom(g.addr, 7)), ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	type accepted struct {
		conn net.Conn
		err  error
	}
	acceptedConn := make(chan accepted, 1)
	go func() {
		conn, err := ln.Accept()
		acceptedConn <- accepted{conn: conn, err: err}
	}()
	exposed, err := g.port.Expose(t.Context(), vm.Forward{HostAddr: "127.0.0.1:0", GuestAddr: ":7"})
	if err != nil {
		t.Fatal(err)
	}
	defer exposed.Close()
	host := exposed.(*exposure).closer.(net.Listener).Addr().String()
	c, err := net.DialTimeout("tcp", host, testTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(testTimeout))
	if _, err := c.Write([]byte("request")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()
	var gc net.Conn
	select {
	case result := <-acceptedConn:
		if result.err != nil {
			t.Fatal(result.err)
		}
		gc = result.conn
	case <-ctx.Done():
		t.Fatal("guest did not accept the exposed connection")
	}
	defer gc.Close() // Keep the guest's write half open throughout exposure teardown.
	_ = gc.SetReadDeadline(time.Now().Add(testTimeout))
	if _, err := io.ReadFull(gc, make([]byte, len("request"))); err != nil {
		t.Fatal(err)
	}
	local := gc.RemoteAddr().(*net.TCPAddr)
	id := stack.TransportEndpointID{
		LocalAddress: addr4(local.AddrPort().Addr()), LocalPort: uint16(local.Port),
		RemoteAddress: addr4(g.addr), RemotePort: 7,
	}
	if ep := n.stack.FindTransportEndpoint(ipv4.ProtocolNumber, tcp.ProtocolNumber, id, nicID); ep == nil {
		t.Fatal("exposed connection has no transport endpoint")
	}
	closed := make(chan error, 1)
	go func() { closed <- exposed.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("exposure close waited for the guest's FIN")
	}
	if ep := n.stack.FindTransportEndpoint(ipv4.ProtocolNumber, tcp.ProtocolNumber, id, nicID); ep != nil {
		t.Fatal("exposure close left an established guest connection behind")
	}
	if err := g.port.Close(); err != nil {
		t.Fatal(err)
	}
	next := attachGuest(t, n, "replacement", vmnet.AttachOptions{Addr: g.addr})
	next.listenTCP(7)
	reopened, err := next.port.Expose(t.Context(), vm.Forward{HostAddr: host, GuestAddr: ":7"})
	if err != nil {
		t.Fatalf("re-expose replacement guest: %v", err)
	}
	defer reopened.Close()
	c2, err := net.DialTimeout("tcp", host, testTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	echo(t, c2, "replacement guest")
}
