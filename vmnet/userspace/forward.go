package userspace

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"

	"github.com/shazow/virtle/internal/dnsproxy"
	"github.com/shazow/virtle/vm"
	"github.com/shazow/virtle/vmnet"
)

// Guest-initiated flows reach the forwarders when no endpoint on the stack
// claims them: everything except the gateway's own services and Listen.
// Each flow is dialed through the Egress before the guest sees it accepted.

func (n *Network) installForwarders() {
	// Select the attachment before gVisor dispatches TCP asynchronously.
	// Each forwarder retains that port, even if its address is later reused.
	n.stack.SetTransportProtocolHandler(tcp.ProtocolNumber, func(id stack.TransportEndpointID, pkt *stack.PacketBuffer) bool {
		if p := n.portByAddr(netipAddr(id.RemoteAddress)); p != nil {
			if id.LocalAddress == n.gateway4 && id.LocalPort == dnsPort {
				return p.dnsTCP.HandlePacket(id, pkt)
			}
			return p.tcpForwarder.HandlePacket(id, pkt)
		}
		return false
	})
	n.stack.SetTransportProtocolHandler(udp.ProtocolNumber, func(id stack.TransportEndpointID, pkt *stack.PacketBuffer) bool {
		if p := n.portByAddr(netipAddr(id.RemoteAddress)); p != nil {
			return p.udpForwarder.HandlePacket(id, pkt)
		}
		return false
	})
}

// errUnknownFakeIP refuses a flow to a synthetic address the DNS never
// handed out: it stands for no name, so nothing can be dialed for it.
var errUnknownFakeIP = errors.New("userspace: no name resolved to the address")

// dialFlow requires a live owning port; an unassigned source never gets the
// network's default policy in place of a guest's restrictions.
func (p *port) dialFlow(proto vm.Proto, id stack.TransportEndpointID, timeout time.Duration) (*forwardedFlow, error) {
	// Guest DNS must use the gateway so its queries are authorized and
	// recorded. The configured resolver is reached separately on the host.
	if id.LocalPort == dnsPort {
		return nil, vmnet.ErrDenied
	}
	f, err := p.newFlow(proto, id)
	if err != nil {
		return nil, err
	}
	n := p.n
	if n.fakeIPs != nil && n.fakeIPs.contains(f.flow.Dst.Addr()) {
		name, ok := n.fakeIPs.name(f.flow.Dst.Addr())
		if !ok {
			f.finish()
			return nil, errUnknownFakeIP
		}
		f.flow.Host = name
	}
	ctx, cancel := context.WithTimeout(p.ctx, timeout)
	defer cancel()
	resolver := n.resolver.WithLogger(n.logger.With("guest", p.name, "src", f.flow.Src))
	upstream, err := n.egress.DialFlow(dnsproxy.WithResolver(ctx, resolver), f.flow)
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

func (p *port) flow(proto vm.Proto, id stack.TransportEndpointID) vmnet.Flow {
	return vmnet.Flow{
		Proto:  proto,
		Src:    netip.AddrPortFrom(p.addr, id.RemotePort),
		Dst:    netip.AddrPortFrom(netipAddr(id.LocalAddress), id.LocalPort),
		Guest:  p.name,
		Egress: p.egress,
	}
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
func (p *port) handleTCP(r *tcp.ForwarderRequest) {
	n := p.n
	if r.ID().LocalAddress == n.gateway4 && r.ID().LocalPort == dnsPort {
		n.dns.acceptTCP(p, r)
		return
	}
	if !n.forwardable(r.ID()) {
		p.rejectTCP(r)
		return
	}
	f, err := p.dialFlow(vm.TCP, r.ID(), dialTimeout)
	if err != nil {
		n.logger.Debug("flow refused", "proto", "tcp", "err", err)
		p.rejectTCP(r)
		return
	}
	defer f.finish()
	c, err := f.acceptTCP(r)
	if err != nil {
		n.logger.Debug("flow endpoint failed", "guest", f.flow.Guest, "dst", f.flow.Dst, "err", err)
		return
	}
	n.logger.Debug("flow opened", "guest", f.flow.Guest, "proto", "tcp", "dst", f.flow.Dst)
	splice(c, f.upstream)
}

// handleUDP runs on the packet path. Returning false leaves the datagram
// unhandled, which the stack answers with an ICMP port unreachable.
func (p *port) handleUDP(r *udp.ForwarderRequest) bool {
	n := p.n
	if !n.forwardable(r.ID()) {
		return false
	}
	f, err := p.dialFlow(vm.UDP, r.ID(), udpDialTimeout)
	if err != nil {
		n.logger.Debug("flow refused", "proto", "udp", "err", err)
		return false
	}
	c, err := f.acceptUDP(r)
	if err != nil {
		n.logger.Debug("flow endpoint failed", "guest", f.flow.Guest, "dst", f.flow.Dst, "err", err)
		f.finish()
		return !errors.Is(err, net.ErrClosed)
	}
	n.logger.Debug("flow opened", "guest", f.flow.Guest, "proto", "udp", "dst", f.flow.Dst)
	go func() {
		defer f.finish()
		relayDatagrams(c, f.upstream)
	}()
	return true
}
