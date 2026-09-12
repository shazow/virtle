package userspace

import (
	"io"
	"net"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"

	"github.com/shazow/virtle/vm"
)

// dnsAddr carries the attachment that received a request through the DNS
// server's asynchronous dispatch. Looking up its IP in ServeDNS would let
// a queued request inherit a later guest's policy after address reuse.
type dnsAddr struct {
	net.Addr
	p *port
}

// dnsUDPEndpoint preserves the receive time gonet would otherwise discard.
// UDP dates each complete datagram when it enters the endpoint's queue.
type dnsUDPEndpoint struct {
	tcpip.Endpoint
	received time.Time
}

func (e *dnsUDPEndpoint) Read(w io.Writer, opts tcpip.ReadOptions) (tcpip.ReadResult, tcpip.Error) {
	r, err := e.Endpoint.Read(w, opts)
	e.received = r.ControlMessages.Timestamp
	return r, err
}

type dnsPacketConn struct {
	*gonet.UDPConn
	n  *Network
	ep *dnsUDPEndpoint
	mu sync.Mutex // keeps each ReadFrom paired with its endpoint timestamp
}

func (c *dnsPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k, addr, err := c.UDPConn.ReadFrom(b)
	if err != nil {
		return k, addr, err
	}
	p := c.n.portByAddr(addr.(*net.UDPAddr).AddrPort().Addr())
	if p != nil && !c.ep.received.After(p.attached) {
		p = nil // This datagram was queued before the current attachment.
	}
	return k, &dnsAddr{Addr: addr, p: p}, nil
}

func (c *dnsPacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	owner, ok := addr.(*dnsAddr)
	if !ok || owner.p == nil {
		return 0, net.ErrClosed
	}
	owner.p.ingress.Lock()
	defer owner.p.ingress.Unlock()
	if owner.p.isClosed() {
		return 0, net.ErrClosed
	}
	return c.UDPConn.WriteTo(b, owner.Addr)
}

// dnsListener receives already established connections from per-port TCP
// forwarders. Capturing the port before asynchronous SYN processing avoids
// a shared listener backlog surviving detach and adopting a new guest.
type dnsListener struct {
	addr  net.Addr
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func (l *dnsListener) Addr() net.Addr { return l.addr }

func (l *dnsListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *dnsListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

type dnsTCPConn struct {
	net.Conn
	f    *forwardedFlow
	addr net.Addr
}

func (c *dnsTCPConn) RemoteAddr() net.Addr { return c.addr }

func (c *dnsTCPConn) Close() error {
	c.f.finish()
	return c.Conn.Close()
}

func (s *dnsServer) acceptTCP(p *port, r *tcp.ForwarderRequest) {
	f, err := p.newFlow(vm.TCP, r.ID())
	if err != nil {
		r.Complete(false)
		return
	}
	c, err := f.acceptTCP(r)
	if err != nil {
		f.finish()
		return
	}
	conn := &dnsTCPConn{Conn: c, f: f, addr: &dnsAddr{Addr: c.RemoteAddr(), p: p}}
	select {
	case s.tcpLn.conns <- conn:
	case <-p.ctx.Done():
		_ = conn.Close()
	case <-s.tcpLn.done:
		_ = conn.Close()
	}
}
