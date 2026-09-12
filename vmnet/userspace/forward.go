package userspace

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"

	"github.com/shazow/virtle/vm"
	"github.com/shazow/virtle/vmnet"
)

// Guest-initiated flows reach the forwarders when no endpoint on the stack
// claims them: everything except the gateway's own services and Listen.
// Each flow is dialed through the Egress before the guest sees it accepted.

func (n *Network) installForwarders() {
	tf := tcp.NewForwarder(n.stack, 0, maxInFlight, n.handleTCP)
	n.stack.SetTransportProtocolHandler(tcp.ProtocolNumber, tf.HandlePacket)
	uf := udp.NewForwarder(n.stack, n.handleUDP)
	n.stack.SetTransportProtocolHandler(udp.ProtocolNumber, uf.HandlePacket)
}

// errUnknownFakeIP refuses a flow to a synthetic address the DNS never
// handed out: it stands for no name, so nothing can be dialed for it.
var errUnknownFakeIP = errors.New("userspace: no name resolved to the address")

// forwardedFlow owns both ends of a guest-initiated flow. It is registered
// before dialing, so detaching the port cancels pending dials and closes
// existing flows before the port's address can be assigned again.
type forwardedFlow struct {
	p    *port
	id   stack.TransportEndpointID
	flow vmnet.Flow

	mu        sync.Mutex
	closed    bool
	upstream  net.Conn
	endpoint  tcpip.Endpoint
	creating  bool
	published chan struct{}
	publish   sync.Once
	closeOnce sync.Once
}

func (f *forwardedFlow) close() { f.closeOnce.Do(f.abort) }

func (f *forwardedFlow) abort() {
	f.mu.Lock()
	f.closed = true
	upstream, creating := f.upstream, f.creating
	f.mu.Unlock()
	if upstream != nil {
		_ = upstream.Close()
	}
	if creating {
		// Wait only for endpoint publication, never for the guest's ACK.
		// The link signals before admitting SYN-ACK to its output queue.
		<-f.published
	}
	f.mu.Lock()
	ep := f.endpoint
	f.mu.Unlock()
	if ep != nil {
		ep.Abort()
	} else if creating {
		if ep := f.p.n.stack.FindTransportEndpoint(ipv4.ProtocolNumber, tcp.ProtocolNumber, f.id, nicID); ep != nil {
			ep.Abort()
		}
	}
}

func (f *forwardedFlow) markPublished() { f.publish.Do(func() { close(f.published) }) }

func (f *forwardedFlow) finish() {
	f.close()
	f.p.mu.Lock()
	delete(f.p.flows, f)
	f.p.mu.Unlock()
}

// dialFlow requires a live owning port; an unassigned source never gets the
// network's default policy in place of a guest's restrictions.
func (n *Network) dialFlow(proto vm.Proto, id stack.TransportEndpointID, timeout time.Duration) (*forwardedFlow, error) {
	p := n.portByAddr(netipAddr(id.RemoteAddress))
	if p == nil {
		return nil, vmnet.ErrDenied
	}
	f := &forwardedFlow{p: p, id: id, published: make(chan struct{}), flow: vmnet.Flow{
		Proto:  proto,
		Src:    netip.AddrPortFrom(p.addr, id.RemotePort),
		Dst:    netip.AddrPortFrom(netipAddr(id.LocalAddress), id.LocalPort),
		Guest:  p.name,
		Egress: p.egress,
	}}
	if n.fakeIPs.contains(f.flow.Dst.Addr()) {
		name, ok := n.fakeIPs.name(f.flow.Dst.Addr())
		if !ok {
			return nil, errUnknownFakeIP
		}
		f.flow.Host = name
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, net.ErrClosed
	}
	p.flows[f] = struct{}{}
	p.mu.Unlock()
	ctx, cancel := context.WithTimeout(p.ctx, timeout)
	defer cancel()
	upstream, err := n.egress.DialFlow(ctx, f.flow)
	f.mu.Lock()
	if err == nil && f.closed {
		err = net.ErrClosed
	}
	if err == nil {
		f.upstream = upstream
	}
	f.mu.Unlock()
	if err != nil {
		if upstream != nil {
			_ = upstream.Close()
		}
		f.finish()
		return nil, err
	}
	return f, nil
}

// forwardable reports whether a flow is one an Egress should see: unicast
// from a guest to somewhere beyond the gateway. The stack loops its own
// broadcasts (DHCP answers) back to itself; guests broadcast and multicast
// among themselves (mDNS, SSDP); and a gateway port with no service behind
// it is closed, not a way out. None of these leave the segment.
func (n *Network) forwardable(id stack.TransportEndpointID) bool {
	dst := id.LocalAddress
	if id.RemoteAddress == n.gateway4 || dst == n.gateway4 || dst == header.IPv4Broadcast || dst == addr4(n.broadcast) {
		return false
	}
	return !header.IsV4MulticastAddress(dst)
}

// handleTCP runs in its own goroutine per SYN. A refused dial resets the
// connection before the handshake completes.
func (n *Network) handleTCP(r *tcp.ForwarderRequest) {
	if !n.forwardable(r.ID()) {
		r.Complete(true)
		return
	}
	f, err := n.dialFlow(vm.TCP, r.ID(), dialTimeout)
	if err != nil {
		n.logger.Debug("flow refused", "proto", "tcp", "err", err)
		r.Complete(true)
		return
	}
	defer f.finish()
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		r.Complete(true)
		return
	}
	f.creating = true
	f.mu.Unlock()
	var wq waiter.Queue
	ep, terr := r.CreateEndpoint(&wq)
	f.markPublished() // Also wake Close when creation fails before SYN-ACK.
	r.Complete(false)
	if terr != nil {
		n.logger.Debug("flow endpoint failed", "guest", f.flow.Guest, "dst", f.flow.Dst, "err", terr.String())
		return
	}
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		ep.Abort()
		return
	}
	f.endpoint = ep
	f.mu.Unlock()
	n.logger.Debug("flow opened", "guest", f.flow.Guest, "proto", "tcp", "dst", f.flow.Dst)
	splice(gonet.NewTCPConn(&wq, ep), f.upstream)
}

// handleUDP runs on the packet path. Returning false leaves the datagram
// unhandled, which the stack answers with an ICMP port unreachable.
func (n *Network) handleUDP(r *udp.ForwarderRequest) bool {
	if !n.forwardable(r.ID()) {
		return false
	}
	f, err := n.dialFlow(vm.UDP, r.ID(), udpDialTimeout)
	if err != nil {
		n.logger.Debug("flow refused", "proto", "udp", "err", err)
		return false
	}
	// UDP endpoint creation does not wait on the guest. Publish it while
	// holding the flow lock so Close cannot miss a newly registered tuple.
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		f.finish()
		return false
	}
	var wq waiter.Queue
	ep, terr := r.CreateEndpoint(&wq)
	if terr == nil {
		f.endpoint = ep
	}
	f.mu.Unlock()
	if terr != nil {
		n.logger.Debug("flow endpoint failed", "guest", f.flow.Guest, "dst", f.flow.Dst, "err", terr.String())
		f.finish()
		return true
	}
	n.logger.Debug("flow opened", "guest", f.flow.Guest, "proto", "udp", "dst", f.flow.Dst)
	go func() {
		defer f.finish()
		relayDatagrams(gonet.NewUDPConn(&wq, ep), f.upstream)
	}()
	return true
}

// splice copies in both directions until both ends are done. A finished
// direction half-closes its destination when it can, so a peer that sends
// EOF and then reads still gets its answer.
func splice(a, b net.Conn) {
	var wg sync.WaitGroup
	closeBoth := sync.OnceFunc(func() {
		_ = a.Close()
		_ = b.Close()
	})
	copy := func(dst, src net.Conn) {
		defer wg.Done()
		_, err := io.Copy(dst, src)
		if err == nil {
			if cw, ok := dst.(interface{ CloseWrite() error }); ok {
				_ = cw.CloseWrite()
				return
			}
		}
		closeBoth()
	}
	wg.Add(2)
	go copy(a, b)
	go copy(b, a)
	wg.Wait()
	closeBoth()
}

// relayDatagrams copies datagrams in both directions until either side
// fails or stays idle for udpIdleTimeout.
func relayDatagrams(a, b net.Conn) {
	var wg sync.WaitGroup
	closeBoth := sync.OnceFunc(func() {
		_ = a.Close()
		_ = b.Close()
	})
	copy := func(dst, src net.Conn) {
		defer wg.Done()
		defer closeBoth()
		buf := make([]byte, maxMTU)
		for {
			_ = src.SetReadDeadline(time.Now().Add(udpIdleTimeout))
			k, err := src.Read(buf)
			if err != nil {
				return
			}
			if _, err := dst.Write(buf[:k]); err != nil {
				return
			}
		}
	}
	wg.Add(2)
	go copy(a, b)
	go copy(b, a)
	wg.Wait()
}

// udpProxy relays datagrams from a host packet conn to per-peer guest
// conns, tracking peers by source address as a NAT would.
type udpProxy struct {
	pc   net.PacketConn
	dial func() (net.Conn, error)

	mu     sync.Mutex
	closed bool
	peers  map[string]net.Conn
}

func newUDPProxy(pc net.PacketConn, dial func() (net.Conn, error)) *udpProxy {
	return &udpProxy{pc: pc, dial: dial, peers: make(map[string]net.Conn)}
}

func (u *udpProxy) run() {
	buf := make([]byte, maxMTU)
	for {
		k, peer, err := u.pc.ReadFrom(buf)
		if err != nil {
			return
		}
		c := u.peer(peer)
		if c == nil {
			continue
		}
		if _, err := c.Write(buf[:k]); err != nil {
			u.drop(peer.String(), c)
		}
	}
}

// peer returns the guest conn for a host peer, dialing one on first use.
func (u *udpProxy) peer(peer net.Addr) net.Conn {
	key := peer.String()
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed {
		return nil
	}
	if c, ok := u.peers[key]; ok {
		return c
	}
	c, err := u.dial()
	if err != nil {
		return nil
	}
	u.peers[key] = c
	go u.reply(peer, c)
	return c
}

// reply copies the guest's answers back to the peer until the flow idles.
func (u *udpProxy) reply(peer net.Addr, c net.Conn) {
	defer u.drop(peer.String(), c)
	buf := make([]byte, maxMTU)
	for {
		_ = c.SetReadDeadline(time.Now().Add(udpIdleTimeout))
		k, err := c.Read(buf)
		if err != nil {
			return
		}
		if _, err := u.pc.WriteTo(buf[:k], peer); err != nil {
			return
		}
	}
}

func (u *udpProxy) drop(key string, c net.Conn) {
	u.mu.Lock()
	if u.peers[key] == c {
		delete(u.peers, key)
	}
	u.mu.Unlock()
	_ = c.Close()
}

func (u *udpProxy) Close() error {
	u.mu.Lock()
	u.closed = true
	peers := make([]net.Conn, 0, len(u.peers))
	for _, c := range u.peers {
		peers = append(peers, c)
	}
	u.peers = map[string]net.Conn{}
	u.mu.Unlock()
	err := u.pc.Close()
	for _, c := range peers {
		_ = c.Close()
	}
	return err
}
