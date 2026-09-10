package userspace

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"

	"github.com/shazow/virtle/vm"
	"github.com/shazow/virtle/vmnet"
)

// port is one attached guest NIC.
type port struct {
	n     *Network
	name  string
	addr  netip.Addr
	addr4 tcpip.Address
	mac   net.HardwareAddr
	link  vmnet.Link
	out   chan []byte   // frames for the guest
	done  chan struct{} // closed with the port

	mu        sync.Mutex
	closed    bool
	exposures map[forwardKey]*exposure

	dropped     atomic.Uint64 // frames the guest did not read in time
	rejected    atomic.Uint64 // frames with a source that is not the guest's
	warnedSpoof atomic.Bool
	warnedDrop  atomic.Bool
}

func (p *port) Addr() netip.Addr { return p.addr }

func (p *port) MAC() net.HardwareAddr { return slices.Clone(p.mac) }

// enqueue hands a frame to the guest's writer, dropping it when the guest
// has fallen behind.
func (p *port) enqueue(frame []byte) {
	select {
	case p.out <- frame:
	default:
		p.dropped.Add(1)
		p.warnOnce(&p.warnedDrop, "dropping frames the guest does not read in time")
	}
}

func (p *port) warnOnce(flag *atomic.Bool, msg string) {
	if flag.CompareAndSwap(false, true) {
		p.n.logger.Warn(msg, "guest", p.name, "addr", p.addr)
	}
}

func (p *port) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// Close implements vmnet.Port.
func (p *port) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	exposures := make([]*exposure, 0, len(p.exposures))
	for _, e := range p.exposures {
		exposures = append(exposures, e)
	}
	close(p.done)
	p.mu.Unlock()
	for _, e := range exposures {
		_ = e.Close()
	}
	_ = p.n.stack.RemoveNeighbor(nicID, ipv4.ProtocolNumber, p.addr4)
	p.n.forget(p)
	err := p.link.Close()
	p.n.logger.Info("network port closed", "guest", p.name, "addr", p.addr,
		"dropped", p.dropped.Load(), "rejected", p.rejected.Load())
	return err
}

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
	e := &exposure{p: p, key: key}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, net.ErrClosed
	}
	if _, dup := p.exposures[key]; dup {
		p.mu.Unlock()
		return nil, fmt.Errorf("userspace: forward %s %s -> %d is already exposed", key.proto, key.host, key.guest)
	}
	p.exposures[key] = e
	p.n.wg.Add(1) // for the serving goroutine listen starts
	p.mu.Unlock()
	if err := e.listen(ctx); err != nil {
		p.mu.Lock()
		delete(p.exposures, key)
		p.mu.Unlock()
		p.n.wg.Done()
		return nil, err
	}
	return e, nil
}

func (p *port) forwardKey(f vm.Forward) (forwardKey, error) {
	proto := f.Proto
	if proto == "" {
		proto = vm.TCP
	}
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

// exposure is one host listener relaying to a guest port.
type exposure struct {
	p   *port
	key forwardKey

	mu     sync.Mutex
	closed bool
	closer io.Closer         // the host listener or packet conn
	conns  map[net.Conn]bool // TCP connections in flight
	proxy  *udpProxy
	once   sync.Once
}

func (e *exposure) listen(ctx context.Context) error {
	guest := tcpip.FullAddress{NIC: nicID, Addr: e.p.addr4, Port: e.key.guest}
	var lc net.ListenConfig
	switch e.key.proto {
	case vm.UDP:
		pc, err := lc.ListenPacket(ctx, "udp", e.key.host)
		if err != nil {
			return fmt.Errorf("userspace: forward: %w", err)
		}
		e.proxy = newUDPProxy(pc, func() (net.Conn, error) {
			return gonet.DialUDP(e.p.n.stack, nil, &guest, ipv4.ProtocolNumber)
		})
		e.closer = e.proxy
		go func() {
			defer e.p.n.wg.Done()
			e.proxy.run()
		}()
	default:
		ln, err := lc.Listen(ctx, "tcp", e.key.host)
		if err != nil {
			return fmt.Errorf("userspace: forward: %w", err)
		}
		e.closer = ln
		e.conns = make(map[net.Conn]bool)
		go func() {
			defer e.p.n.wg.Done()
			e.serveTCP(ln, guest)
		}()
	}
	return nil
}

func (e *exposure) serveTCP(ln net.Listener, guest tcpip.FullAddress) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		if !e.track(c) {
			_ = c.Close()
			return
		}
		go func() {
			defer e.untrack(c)
			ctx, cancel := context.WithTimeout(e.p.n.ctx, dialTimeout)
			gc, err := gonet.DialContextTCP(ctx, e.p.n.stack, guest, ipv4.ProtocolNumber)
			cancel()
			if err != nil {
				e.p.n.logger.Debug("forward: guest refused", "guest", e.p.name, "port", e.key.guest, "err", err)
				_ = c.Close()
				return
			}
			splice(c, gc)
		}()
	}
}

func (e *exposure) track(c net.Conn) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return false
	}
	e.conns[c] = true
	return true
}

func (e *exposure) untrack(c net.Conn) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.conns, c)
}

// Close stops the listener and ends the connections it accepted.
func (e *exposure) Close() error {
	var err error
	e.once.Do(func() {
		e.p.mu.Lock()
		if e.p.exposures[e.key] == e {
			delete(e.p.exposures, e.key)
		}
		e.p.mu.Unlock()
		e.mu.Lock()
		e.closed = true
		conns := make([]net.Conn, 0, len(e.conns))
		for c := range e.conns {
			conns = append(conns, c)
		}
		e.mu.Unlock()
		err = e.closer.Close()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	return err
}

var _ vmnet.Port = (*port)(nil)
