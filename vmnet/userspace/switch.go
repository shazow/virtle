package userspace

import (
	"errors"
	"io"
	"net"
	"slices"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"

	"github.com/shazow/virtle/vm"
	"github.com/shazow/virtle/vmnet"
)

// networkEndpoint observes TCP endpoint publication before an outgoing
// packet can be dropped. gVisor registers a passive TCP endpoint before
// writing SYN-ACK, but CreateEndpoint returns only after the handshake.
// This signal lets port teardown abort that endpoint without waiting on
// the guest or racing an endpoint that has not been registered yet.
type networkEndpoint struct {
	*channel.Endpoint
	n *Network
}

func (e *networkEndpoint) WritePackets(pkts stack.PacketBufferList) (int, tcpip.Error) {
	hasDNS := false
	for _, pkt := range pkts.AsSlice() {
		if gatewayDNSReply(e.n, pkt) {
			hasDNS = true
		}
		if pkt.TransportProtocolNumber != tcp.ProtocolNumber {
			continue
		}
		h := header.TCP(pkt.TransportHeader().Slice())
		if !h.Flags().Contains(header.TCPFlagSyn | header.TCPFlagAck) {
			continue
		}
		ip := pkt.Network()
		p := e.n.portByAddr(netipAddr(ip.DestinationAddress()))
		if p == nil {
			continue
		}
		p.mu.Lock()
		for f := range p.flows {
			if f.flow.Proto == vm.TCP && f.id.LocalAddress == ip.SourceAddress() && f.id.LocalPort == h.SourcePort() && f.id.RemotePort == h.DestinationPort() {
				f.markPublished()
			}
		}
		p.mu.Unlock()
	}
	if hasDNS {
		written := 0
		for _, pkt := range pkts.AsSlice() {
			if gatewayDNSReply(e.n, pkt) {
				// UDP WriteTo holds the owner's ingress barrier. Hand its
				// reply directly to that port: the shared switch queue could
				// otherwise deliver it after its address and MAC are reused.
				if p := e.n.portByAddr(netipAddr(pkt.Network().DestinationAddress())); p != nil {
					view := pkt.ToView()
					p.enqueue(slices.Clone(view.AsSlice()))
					view.Release()
				}
				written++
				continue
			}
			var one stack.PacketBufferList
			one.PushBack(pkt)
			n, err := e.Endpoint.WritePackets(one)
			written += n
			if err != nil || n == 0 {
				return written, err
			}
		}
		return written, nil
	}
	return e.Endpoint.WritePackets(pkts)
}

func gatewayDNSReply(n *Network, pkt *stack.PacketBuffer) bool {
	return pkt.NetworkProtocolNumber == header.IPv4ProtocolNumber &&
		pkt.TransportProtocolNumber == header.UDPProtocolNumber &&
		len(pkt.TransportHeader().Slice()) >= header.UDPMinimumSize &&
		pkt.Network().SourceAddress() == n.gateway4 &&
		header.UDP(pkt.TransportHeader().Slice()).SourcePort() == dnsPort
}

// The switch moves Ethernet frames between the stack's link endpoint and the
// attached ports. Every port's MAC is known at attach, so there is no
// learning: unicast goes to the owning port, broadcast and multicast to all
// of them, and a frame for an unknown MAC is dropped rather than flooded.

// runSwitch delivers frames the stack sends.
func (n *Network) runSwitch() {
	defer n.wg.Done()
	for {
		pkt := n.ep.ReadContext(n.ctx)
		if pkt == nil {
			return
		}
		view := pkt.ToView()
		frame := slices.Clone(view.AsSlice())
		view.Release()
		pkt.DecRef()
		n.fromStack(frame)
	}
}

func (n *Network) fromStack(frame []byte) {
	if len(frame) < header.EthernetMinimumSize {
		return
	}
	eth := header.Ethernet(frame)
	if eth.Type() == header.ARPProtocolNumber && !n.ownARPReply(frame[header.EthernetMinimumSize:]) {
		return
	}
	dst := eth.DestinationAddress()
	if isGroup(dst) {
		for _, p := range n.ports() {
			p.enqueue(slices.Clone(frame))
		}
		return
	}
	if p := n.portByMAC(dst); p != nil {
		p.enqueue(frame)
	}
}

// fromGuest handles one frame read from a port's link. Frames that do not
// come from the port's own MAC and address are dropped, so a guest cannot
// speak for another one or for the gateway.
func (n *Network) fromGuest(p *port, frame []byte) {
	if p.isClosed() {
		return
	}
	if len(frame) < header.EthernetMinimumSize {
		return
	}
	eth := header.Ethernet(frame)
	switch eth.Type() {
	case header.IPv4ProtocolNumber, header.ARPProtocolNumber:
	default:
		return // IPv6 and the rest: the segment carries IPv4 only
	}
	if eth.SourceAddress() != tcpip.LinkAddress(p.mac) || !p.ownsSource(eth.Type(), frame[header.EthernetMinimumSize:]) {
		p.rejected.Add(1)
		p.warnOnce(&p.warnedSpoof, "dropping frames from the guest with a source that is not its own")
		return
	}
	if eth.Type() == header.IPv4ProtocolNumber {
		ip := header.IPv4(frame[header.EthernetMinimumSize:])
		if ip.DestinationAddress() == n.gateway4 {
			// Gateway services must not inherit partial datagrams across
			// address reuse. DNS can use TCP for messages above the MTU.
			if (ip.Protocol() == uint8(header.TCPProtocolNumber) || ip.Protocol() == uint8(header.UDPProtocolNumber)) &&
				(ip.FragmentOffset() != 0 || ip.Flags()&header.IPv4FlagMoreFragments != 0) {
				p.rejected.Add(1)
				return
			}
		}
		// TODO: Bind external UDP admission and fragment reassembly to the
		// originating attachment without holding ingress across Egress dialing.
		if ip.DestinationAddress() == n.gateway4 || ip.Protocol() == uint8(header.TCPProtocolNumber) {
			// TCP selects its port synchronously, then dispatches the SYN
			// handler asynchronously. Keep that selection within the
			// attachment's lifetime, including external destinations.
			p.ingress.Lock()
			defer p.ingress.Unlock()
			if p.isClosed() {
				return
			}
		}
		// Drop the first fragment as well as whole packets; without it a
		// peer cannot reassemble a DNS query that bypasses the gateway.
		if ip.FragmentOffset() == 0 && ip.DestinationAddress() != n.gateway4 &&
			(ip.Protocol() == uint8(header.TCPProtocolNumber) || ip.Protocol() == uint8(header.UDPProtocolNumber)) {
			payload := ip.Payload()
			if len(payload) >= 4 && uint16(payload[2])<<8|uint16(payload[3]) == dnsPort {
				p.rejected.Add(1)
				return
			}
		}
	}
	dst := eth.DestinationAddress()
	switch {
	case dst == tcpip.LinkAddress(n.gatewayHW):
		n.toStack(frame)
	case isGroup(dst):
		for _, q := range n.ports() {
			if q != p {
				q.enqueue(slices.Clone(frame))
			}
		}
		n.toStack(frame)
	default:
		if q := n.portByMAC(dst); q != nil {
			q.enqueue(slices.Clone(frame))
		}
	}
}

// ownARPReply reports whether an ARP packet from the stack may reach the
// guests. In spoofing mode the stack answers ARP for every address, which
// would make it the next hop for guest-to-guest traffic; only replies about
// the gateway's own address pass.
func (n *Network) ownARPReply(payload []byte) bool {
	a := header.ARP(payload)
	if !a.IsValid() {
		return false
	}
	return a.Op() != header.ARPReply || tcpip.AddrFrom4Slice(a.ProtocolAddressSender()) == n.gateway4
}

// ownsSource reports whether the network-layer source in a guest's frame is
// the port's address, or unspecified as during DHCP and ARP probes.
func (p *port) ownsSource(proto tcpip.NetworkProtocolNumber, payload []byte) bool {
	switch proto {
	case header.IPv4ProtocolNumber:
		ip := header.IPv4(payload)
		if !ip.IsValid(len(payload)) {
			return false
		}
		src := ip.SourceAddress()
		return src == p.addr4 || src == header.IPv4Any
	case header.ARPProtocolNumber:
		a := header.ARP(payload)
		if !a.IsValid() {
			return false
		}
		src := tcpip.AddrFrom4Slice(a.ProtocolAddressSender())
		return string(a.HardwareAddressSender()) == string(p.mac) && (src == p.addr4 || src == header.IPv4Any)
	}
	return false
}

// toStack injects a guest's frame into the stack, which parses the
// Ethernet header itself.
func (n *Network) toStack(frame []byte) {
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(slices.Clone(frame))})
	n.ep.InjectInbound(header.IPv4ProtocolNumber, pkt)
	pkt.DecRef()
}

// portRx reads frames from a port's link until it fails, which closes the
// port.
func (n *Network) portRx(p *port) {
	defer n.wg.Done()
	buf := make([]byte, p.link.MTU()+vmnet.EthernetHeader)
	for {
		k, err := p.link.ReadFrame(buf)
		if err != nil {
			if errors.Is(err, io.ErrShortBuffer) {
				continue
			}
			if !p.isClosed() && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				n.logger.Debug("network link read failed", "guest", p.name, "err", err)
			}
			_ = p.Close()
			return
		}
		n.fromGuest(p, buf[:k])
	}
}

// portTx writes queued frames to a port's link.
func (n *Network) portTx(p *port) {
	defer n.wg.Done()
	for {
		select {
		case <-p.done:
			return
		case frame := <-p.out:
			if err := p.link.WriteFrame(frame); err != nil {
				if !p.isClosed() {
					n.logger.Debug("network link write failed", "guest", p.name, "err", err)
				}
				_ = p.Close()
				return
			}
		}
	}
}

func (n *Network) ports() []*port {
	n.mu.Lock()
	defer n.mu.Unlock()
	ps := make([]*port, 0, len(n.byAddr))
	for _, p := range n.byAddr {
		ps = append(ps, p)
	}
	return ps
}

func (n *Network) portByMAC(mac tcpip.LinkAddress) *port {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.byMAC[net.HardwareAddr(mac).String()]
}

// isGroup reports a broadcast or multicast destination.
func isGroup(mac tcpip.LinkAddress) bool { return len(mac) == 6 && mac[0]&1 != 0 }
