package userspace

import (
	"context"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
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

// flowFor describes a forwarder request; the guest is found by its source
// address, which the switch has already checked belongs to it.
func (n *Network) flowFor(proto vm.Proto, id stack.TransportEndpointID) vmnet.Flow {
	f := vmnet.Flow{
		Proto: proto,
		Src:   netip.AddrPortFrom(netipAddr(id.RemoteAddress), id.RemotePort),
		Dst:   netip.AddrPortFrom(netipAddr(id.LocalAddress), id.LocalPort),
	}
	if p := n.portByAddr(f.Src.Addr()); p != nil {
		f.Guest = p.name
	}
	return f
}

// forwardable reports whether a flow is one an Egress should see: unicast
// from a guest. The stack loops its own broadcasts (DHCP answers) back to
// itself, and guests broadcast and multicast among themselves (mDNS, SSDP);
// neither leaves the segment.
func (n *Network) forwardable(id stack.TransportEndpointID) bool {
	dst := id.LocalAddress
	if id.RemoteAddress == n.gateway4 || dst == header.IPv4Broadcast || dst == addr4(n.broadcast) {
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
	flow := n.flowFor(vm.TCP, r.ID())
	ctx, cancel := context.WithTimeout(n.ctx, dialTimeout)
	upstream, err := n.egress.DialFlow(ctx, flow)
	cancel()
	if err != nil {
		n.logger.Debug("flow refused", "guest", flow.Guest, "proto", "tcp", "dst", flow.Dst, "err", err)
		r.Complete(true)
		return
	}
	var wq waiter.Queue
	ep, terr := r.CreateEndpoint(&wq)
	r.Complete(false)
	if terr != nil {
		n.logger.Debug("flow endpoint failed", "guest", flow.Guest, "dst", flow.Dst, "err", terr.String())
		_ = upstream.Close()
		return
	}
	n.logger.Debug("flow opened", "guest", flow.Guest, "proto", "tcp", "dst", flow.Dst)
	splice(gonet.NewTCPConn(&wq, ep), upstream)
}

// handleUDP runs on the packet path. Returning false leaves the datagram
// unhandled, which the stack answers with an ICMP port unreachable.
func (n *Network) handleUDP(r *udp.ForwarderRequest) bool {
	if !n.forwardable(r.ID()) {
		return false
	}
	flow := n.flowFor(vm.UDP, r.ID())
	ctx, cancel := context.WithTimeout(n.ctx, udpDialTimeout)
	upstream, err := n.egress.DialFlow(ctx, flow)
	cancel()
	if err != nil {
		n.logger.Debug("flow refused", "guest", flow.Guest, "proto", "udp", "dst", flow.Dst, "err", err)
		return false
	}
	var wq waiter.Queue
	ep, terr := r.CreateEndpoint(&wq)
	if terr != nil {
		n.logger.Debug("flow endpoint failed", "guest", flow.Guest, "dst", flow.Dst, "err", terr.String())
		_ = upstream.Close()
		return true
	}
	n.logger.Debug("flow opened", "guest", flow.Guest, "proto", "udp", "dst", flow.Dst)
	go relayDatagrams(gonet.NewUDPConn(&wq, ep), upstream, udpIdleTimeout)
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
// fails or stays idle for the timeout.
func relayDatagrams(a, b net.Conn, idle time.Duration) {
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
			_ = src.SetReadDeadline(time.Now().Add(idle))
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
