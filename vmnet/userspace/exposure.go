package userspace

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/waiter"

	"github.com/shazow/virtle/vm"
)

// forwardKey identifies an exposure: the same host address can carry TCP
// and UDP, and one guest port can be reached from several host addresses.
type forwardKey struct {
	proto vm.Proto
	host  string
	guest uint16
}

// Expose implements vmnet.Port: it listens on the forward's host address
// and relays each connection or datagram to the guest port.
func (p *port) Expose(ctx context.Context, f vm.Forward) (io.Closer, error) {
	key, err := p.forwardKey(f)
	if err != nil {
		return nil, err
	}
	// Checked before binding for the clearer error, and again when
	// publishing, which is what counts.
	p.mu.Lock()
	err = p.exposableLocked(key)
	p.mu.Unlock()
	if err != nil {
		return nil, err
	}
	lifetime, cancel := context.WithCancel(p.ctx)
	e := &exposure{p: p, key: key, ctx: lifetime, cancel: cancel}
	serve, err := e.bind(ctx)
	if err != nil {
		cancel()
		return nil, err
	}
	// Published only once bound, so a Close that finds the exposure has a
	// listener to close; a port closed meanwhile takes the listener down.
	p.mu.Lock()
	if err := p.exposableLocked(key); err != nil {
		p.mu.Unlock()
		_ = e.Close()
		return nil, err
	}
	p.exposures[key] = e
	e.wg.Add(1)
	p.n.wg.Add(1)
	p.mu.Unlock()
	go func() {
		defer p.n.wg.Done()
		defer e.wg.Done()
		serve()
	}()
	return e, nil
}

// exposableLocked reports whether a forward can be exposed now: the port is
// open and nothing else holds the key.
func (p *port) exposableLocked(key forwardKey) error {
	if p.closed {
		return net.ErrClosed
	}
	if _, dup := p.exposures[key]; dup {
		return fmt.Errorf("userspace: forward %s %s -> %d is already exposed", key.proto, key.host, key.guest)
	}
	return nil
}

func (p *port) forwardKey(f vm.Forward) (forwardKey, error) {
	proto := cmp.Or(f.Proto, vm.TCP)
	if proto != vm.TCP && proto != vm.UDP {
		return forwardKey{}, fmt.Errorf("userspace: forward protocol %q: %w", proto, net.UnknownNetworkError(string(proto)))
	}
	if f.HostAddr == "" {
		return forwardKey{}, fmt.Errorf("userspace: forward to guest port %q has no host address", f.GuestAddr)
	}
	host, portStr, err := net.SplitHostPort(f.GuestAddr)
	if err != nil {
		return forwardKey{}, fmt.Errorf("userspace: forward guest address: %w", err)
	}
	if host != "" {
		a, err := netip.ParseAddr(host)
		if err != nil || a.Unmap() != p.addr {
			return forwardKey{}, fmt.Errorf("userspace: forward guest address %q is not this port's %s", f.GuestAddr, p.addr)
		}
	}
	guestPort, err := net.LookupPort(string(proto), portStr)
	if err != nil {
		return forwardKey{}, fmt.Errorf("userspace: forward guest address %q: %w", f.GuestAddr, err)
	}
	if guestPort == 0 {
		return forwardKey{}, fmt.Errorf("userspace: forward guest address %q has no port", f.GuestAddr)
	}
	return forwardKey{proto: proto, host: f.HostAddr, guest: uint16(guestPort)}, nil
}

// exposure owns a host listener and all connections accepted through it.
// Its lifetime ends with either the exposure or its attached port.
type exposure struct {
	p      *port
	key    forwardKey
	ctx    context.Context
	cancel context.CancelFunc
	closer io.Closer // the host listener or UDP proxy

	mu       sync.Mutex
	closed   bool
	conns    map[*exposedTCPConn]struct{}
	wg       sync.WaitGroup // listener and accepted handlers; registration is sealed by closed
	once     sync.Once
	closeErr error
}

// bind takes the host address and returns the function that serves it,
// which runs until the exposure is closed.
func (e *exposure) bind(ctx context.Context) (serve func(), err error) {
	guest := tcpip.FullAddress{NIC: nicID, Addr: e.p.addr4, Port: e.key.guest}
	var lc net.ListenConfig
	switch e.key.proto {
	case vm.UDP:
		pc, err := lc.ListenPacket(ctx, "udp", e.key.host)
		if err != nil {
			return nil, fmt.Errorf("userspace: forward: %w", err)
		}
		proxy := newUDPProxy(pc, func() (net.Conn, error) {
			return gonet.DialUDP(e.p.n.stack, nil, &guest, ipv4.ProtocolNumber)
		})
		e.closer = proxy
		return proxy.run, nil
	default:
		ln, err := lc.Listen(ctx, "tcp", e.key.host)
		if err != nil {
			return nil, fmt.Errorf("userspace: forward: %w", err)
		}
		e.closer = ln
		e.conns = make(map[*exposedTCPConn]struct{})
		return func() { e.serveTCP(ln, guest) }, nil
	}
}

// exposedTCPConn owns the accepted host connection and the guest endpoint
// before its handshake starts. gonet's dial helpers hide that endpoint, but
// owner teardown needs Abort: ordinary Close can leave TCP state waiting
// for the guest's FIN.
type exposedTCPConn struct {
	host     net.Conn
	guest    *gonet.TCPConn
	endpoint tcpip.Endpoint
	wq       waiter.Queue
}

func newExposedTCPConn(s *stack.Stack, host net.Conn) (*exposedTCPConn, error) {
	c := &exposedTCPConn{host: host}
	ep, err := s.NewEndpoint(tcp.ProtocolNumber, ipv4.ProtocolNumber, &c.wq)
	if err != nil {
		return nil, tcpipError("forward TCP endpoint", err)
	}
	c.endpoint = ep
	c.guest = gonet.NewTCPConn(&c.wq, ep)
	return c, nil
}

// connect waits for the owned endpoint's handshake with the same writable
// notification that gonet uses, retaining the endpoint for explicit teardown.
func (c *exposedTCPConn) connect(ctx context.Context, guest tcpip.FullAddress) error {
	entry, ready := waiter.NewChannelEntry(waiter.WritableEvents)
	c.wq.EventRegister(&entry)
	defer c.wq.EventUnregister(&entry)
	if err := ctx.Err(); err != nil {
		c.endpoint.Abort()
		return err
	}
	err := c.endpoint.Connect(guest)
	if _, pending := err.(*tcpip.ErrConnectStarted); pending {
		select {
		case <-ctx.Done():
			c.endpoint.Abort()
			return ctx.Err()
		case <-ready:
		}
		err = c.endpoint.LastError()
	}
	if err != nil {
		return tcpipError("forward TCP connect", err)
	}
	return nil
}

func (e *exposure) serveTCP(ln net.Listener, guest tcpip.FullAddress) {
	for {
		host, err := ln.Accept()
		if err != nil {
			return
		}
		c, err := newExposedTCPConn(e.p.n.stack, host)
		if err != nil {
			_ = host.Close()
			continue
		}
		e.mu.Lock()
		if e.closed {
			e.mu.Unlock()
			c.endpoint.Abort()
			_ = host.Close()
			return
		}
		e.conns[c] = struct{}{}
		e.wg.Add(1)
		e.mu.Unlock()
		go e.forwardTCP(c, guest)
	}
}

func (e *exposure) forwardTCP(c *exposedTCPConn, guest tcpip.FullAddress) {
	defer e.wg.Done()
	defer func() {
		_ = c.host.Close()
		_ = c.guest.Close()
		e.mu.Lock()
		delete(e.conns, c)
		e.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(e.ctx, dialTimeout)
	err := c.connect(ctx, guest)
	cancel()
	if err != nil {
		e.p.n.logger.Debug("forward: guest refused", "guest", e.p.name, "port", e.key.guest, "err", err)
		return
	}
	splice(c.host, c.guest)
}

// Close cancels pending guest dials and joins the listener and its handlers.
// Both established connection ends are closed so a peer cannot hold teardown
// open by leaving its half of a TCP stream alive.
func (e *exposure) Close() error {
	e.once.Do(func() {
		e.mu.Lock()
		e.closed = true
		e.cancel()
		conns := make([]*exposedTCPConn, 0, len(e.conns))
		for c := range e.conns {
			conns = append(conns, c)
		}
		e.mu.Unlock()
		e.closeErr = e.closer.Close()
		for _, c := range conns {
			_ = c.host.Close()
			c.endpoint.Abort()
		}
		e.wg.Wait()
		// Keep the exposure registered through teardown: port.Close must
		// also join an exposure that another caller is already closing.
		e.p.mu.Lock()
		if e.p.exposures[e.key] == e {
			delete(e.p.exposures, e.key)
		}
		e.p.mu.Unlock()
	})
	return e.closeErr
}
