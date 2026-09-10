package userspace

import (
	"context"
	"io"
	"net"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/ethernet"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"

	"github.com/shazow/virtle/vmnet"
)

// guest is a second netstack standing in for a guest kernel behind one
// port: it takes its lease over the link the way a kernel's DHCP client
// would, then dials and listens like a guest.
type guest struct {
	t     *testing.T
	n     *Network
	port  vmnet.Port
	link  vmnet.Link // the guest's end
	stack *stack.Stack
	ep    *channel.Endpoint
	mac   tcpip.LinkAddress
	addr  netip.Addr
	offer *dhcpv4.DHCPv4
	ack   *dhcpv4.DHCPv4
}

const testTimeout = 10 * time.Second

// idleLink is a link whose peer never speaks, for attaching without a guest.
func idleLink(t *testing.T, mtu int) vmnet.Link {
	t.Helper()
	hostEnd, guestEnd := net.Pipe()
	t.Cleanup(func() { _ = guestEnd.Close() })
	return vmnet.QEMUStream(hostEnd, mtu)
}

func newTestNetwork(t *testing.T, cfg Config) *Network {
	t.Helper()
	n, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := n.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return n
}

// attachGuest attaches a guest named name and boots it through DHCP.
func attachGuest(t *testing.T, n *Network, name string, opts vmnet.AttachOptions) *guest {
	t.Helper()
	hostEnd, guestEnd := net.Pipe()
	opts.Name = name
	port, err := n.Attach(context.Background(), vmnet.QEMUStream(hostEnd, n.MTU()), opts)
	if err != nil {
		t.Fatalf("Attach %s: %v", name, err)
	}
	t.Cleanup(func() { _ = port.Close() })
	g := &guest{t: t, n: n, port: port, link: vmnet.QEMUStream(guestEnd, n.MTU()), mac: tcpip.LinkAddress(port.MAC())}
	g.dhcp()
	g.start()
	return g
}

// dhcp runs a discover/offer/request/ack exchange with raw frames, as a
// kernel with no address yet must.
func (g *guest) dhcp() {
	g.t.Helper()
	discover, err := dhcpv4.NewDiscovery(net.HardwareAddr(g.mac))
	if err != nil {
		g.t.Fatal(err)
	}
	g.sendDHCP(discover)
	g.offer = g.awaitDHCP(dhcpv4.MessageTypeOffer)
	request, err := dhcpv4.NewRequestFromOffer(g.offer)
	if err != nil {
		g.t.Fatal(err)
	}
	g.sendDHCP(request)
	g.ack = g.awaitDHCP(dhcpv4.MessageTypeAck)
	addr, ok := netip.AddrFromSlice(g.ack.YourIPAddr.To4())
	if !ok {
		g.t.Fatalf("ack without an address: %s", g.ack.Summary())
	}
	g.addr = addr
}

// sendDHCP broadcasts a client message from the unspecified address.
func (g *guest) sendDHCP(m *dhcpv4.DHCPv4) {
	payload := m.ToBytes()
	udpLen := header.UDPMinimumSize + len(payload)
	frame := make([]byte, header.EthernetMinimumSize+header.IPv4MinimumSize+udpLen)
	header.Ethernet(frame).Encode(&header.EthernetFields{
		SrcAddr: g.mac, DstAddr: header.EthernetBroadcastAddress, Type: header.IPv4ProtocolNumber,
	})
	ip := header.IPv4(frame[header.EthernetMinimumSize:])
	ip.Encode(&header.IPv4Fields{
		TotalLength: uint16(header.IPv4MinimumSize + udpLen),
		TTL:         64,
		Protocol:    uint8(udp.ProtocolNumber),
		SrcAddr:     header.IPv4Any,
		DstAddr:     header.IPv4Broadcast,
	})
	ip.SetChecksum(^ip.CalculateChecksum())
	u := header.UDP(frame[header.EthernetMinimumSize+header.IPv4MinimumSize:])
	u.Encode(&header.UDPFields{SrcPort: dhcpClientPort, DstPort: dhcpServerPort, Length: uint16(udpLen)})
	copy(u.Payload(), payload)
	sum := header.PseudoHeaderChecksum(udp.ProtocolNumber, header.IPv4Any, header.IPv4Broadcast, uint16(udpLen))
	u.SetChecksum(^u.CalculateChecksum(checksum.Checksum(payload, sum)))
	if err := g.link.WriteFrame(frame); err != nil {
		g.t.Fatalf("send DHCP: %v", err)
	}
}

// awaitDHCP reads frames until a server message of the wanted type.
func (g *guest) awaitDHCP(want dhcpv4.MessageType) *dhcpv4.DHCPv4 {
	g.t.Helper()
	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		frame, ok := g.readFrame(time.Until(deadline))
		if !ok {
			break
		}
		const l3 = header.EthernetMinimumSize
		if len(frame) < l3+header.IPv4MinimumSize+header.UDPMinimumSize || header.Ethernet(frame).Type() != header.IPv4ProtocolNumber {
			continue
		}
		ip := header.IPv4(frame[l3:])
		if !ip.IsValid(len(frame)-l3) || ip.Protocol() != uint8(udp.ProtocolNumber) {
			continue
		}
		u := header.UDP(frame[l3+int(ip.HeaderLength()):])
		if u.DestinationPort() != dhcpClientPort || int(u.Length()) > len(u) {
			continue
		}
		m, err := dhcpv4.FromBytes(u.Payload()[:int(u.Length())-header.UDPMinimumSize])
		if err != nil || m.MessageType() != want {
			continue
		}
		return m
	}
	g.t.Fatalf("no DHCP %s within %s", want, testTimeout)
	return nil
}

// readFrame reads one frame from the link or gives up after the timeout.
func (g *guest) readFrame(timeout time.Duration) ([]byte, bool) {
	type result struct {
		frame []byte
		err   error
	}
	ch := make(chan result, 1)
	go func() {
		buf := make([]byte, g.n.MTU()+vmnet.EthernetHeader)
		k, err := g.link.ReadFrame(buf)
		ch <- result{slices.Clone(buf[:k]), err}
	}()
	select {
	case r := <-ch:
		return r.frame, r.err == nil
	case <-time.After(timeout):
		return nil, false
	}
}

// start brings up the guest stack with the lease and pumps frames.
func (g *guest) start() {
	g.t.Helper()
	g.ep = channel.New(64, uint32(g.n.MTU()+header.EthernetMinimumSize), g.mac)
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol4},
	})
	if err := s.CreateNIC(nicID, ethernet.New(g.ep)); err != nil {
		g.t.Fatal(err)
	}
	ones, _ := g.ack.SubnetMask().Size()
	if err := s.AddProtocolAddress(nicID, tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{Address: addr4(g.addr), PrefixLen: ones},
	}, stack.AddressProperties{}); err != nil {
		g.t.Fatal(err)
	}
	subnet, err := tcpip.NewSubnet(addr4(netip.PrefixFrom(g.addr, ones).Masked().Addr()), tcpip.MaskFromBytes(g.ack.SubnetMask()))
	if err != nil {
		g.t.Fatal(err)
	}
	routers := g.ack.Router()
	if len(routers) != 1 {
		g.t.Fatalf("ack routers = %v", routers)
	}
	s.SetRouteTable([]tcpip.Route{
		{Destination: subnet, NIC: nicID},
		{Destination: header.IPv4EmptySubnet, Gateway: tcpip.AddrFromSlice(routers[0].To4()), NIC: nicID},
	})
	g.stack = s
	ctx, cancel := context.WithCancel(context.Background())
	g.t.Cleanup(func() {
		cancel()
		s.Destroy()
	})
	go func() {
		for {
			pkt := g.ep.ReadContext(ctx)
			if pkt == nil {
				return
			}
			view := pkt.ToView()
			frame := slices.Clone(view.AsSlice())
			view.Release()
			pkt.DecRef()
			if err := g.link.WriteFrame(frame); err != nil {
				return
			}
		}
	}()
	go func() {
		buf := make([]byte, g.n.MTU()+vmnet.EthernetHeader)
		for {
			k, err := g.link.ReadFrame(buf)
			if err != nil {
				return
			}
			pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(slices.Clone(buf[:k]))})
			g.ep.InjectInbound(header.IPv4ProtocolNumber, pkt)
			pkt.DecRef()
		}
	}()
}

func full(addr netip.AddrPort) tcpip.FullAddress {
	return tcpip.FullAddress{NIC: nicID, Addr: addr4(addr.Addr()), Port: addr.Port()}
}

func (g *guest) dialTCP(ctx context.Context, addr netip.AddrPort) (net.Conn, error) {
	return gonet.DialContextTCP(ctx, g.stack, full(addr), ipv4.ProtocolNumber)
}

func (g *guest) dialUDP(addr netip.AddrPort) (net.Conn, error) {
	return gonet.DialUDP(g.stack, nil, ptr(full(addr)), ipv4.ProtocolNumber)
}

// listenTCP serves an echo on the guest's address.
func (g *guest) listenTCP(port uint16) net.Listener {
	g.t.Helper()
	ln, err := gonet.ListenTCP(g.stack, full(netip.AddrPortFrom(g.addr, port)), ipv4.ProtocolNumber)
	if err != nil {
		g.t.Fatalf("guest listen: %v", err)
	}
	g.t.Cleanup(func() { _ = ln.Close() })
	go serveEcho(ln)
	return ln
}

// listenUDP binds a guest UDP port that echoes every datagram.
func (g *guest) listenUDP(port uint16) net.PacketConn {
	g.t.Helper()
	pc, err := gonet.DialUDP(g.stack, ptr(full(netip.AddrPortFrom(g.addr, port))), nil, ipv4.ProtocolNumber)
	if err != nil {
		g.t.Fatalf("guest listen udp: %v", err)
	}
	g.t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			k, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteTo(buf[:k], from)
		}
	}()
	return pc
}

func ptr[T any](v T) *T { return &v }

func serveEcho(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			_, _ = io.Copy(c, c)
		}()
	}
}

// echo writes msg on c and expects it back.
func echo(t *testing.T, c net.Conn, msg string) {
	t.Helper()
	_ = c.SetDeadline(time.Now().Add(testTimeout))
	if _, err := c.Write([]byte(msg)); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != msg {
		t.Fatalf("echoed %q, want %q", buf, msg)
	}
}

// freePort finds a TCP or UDP port on the loopback that nothing holds.
func freePort(t *testing.T, network string) string {
	t.Helper()
	switch network {
	case "udp":
		pc, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer pc.Close()
		return pc.LocalAddr().String()
	default:
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		return ln.Addr().String()
	}
}
