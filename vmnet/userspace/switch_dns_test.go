package userspace

import (
	"bytes"
	"context"
	"net/netip"
	"testing"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/stack"

	"github.com/shazow/virtle/vmnet"
)

func TestGuestDNSUsesTheGateway(t *testing.T) {
	n := newTestNetwork(t, Config{Egress: vmnet.DenyAll{}})
	attached, err := n.Attach(context.Background(), idleLink(t, n.MTU()), vmnet.AttachOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p := attached.(*port)
	for _, protocol := range []struct {
		name   string
		number tcpip.TransportProtocolNumber
	}{{"udp", header.UDPProtocolNumber}, {"tcp", header.TCPProtocolNumber}} {
		for _, destination := range []struct {
			name string
			addr netip.Addr
			mac  tcpip.LinkAddress
		}{
			{"gateway", n.gateway, tcpip.LinkAddress(n.gatewayHW)},
			{"peer", p.addr.Next(), tcpip.LinkAddress(macFor(p.addr.Next()))},
			{"external", netip.MustParseAddr("203.0.113.53"), tcpip.LinkAddress(n.gatewayHW)},
			{"broadcast", netip.MustParseAddr("255.255.255.255"), header.EthernetBroadcastAddress},
			{"subnet broadcast", n.broadcast, header.EthernetBroadcastAddress},
			{"multicast", netip.MustParseAddr("224.0.0.251"), tcpip.LinkAddress("\x01\x00\x5e\x00\x00\xfb")},
		} {
			for _, fragment := range []bool{false, true} {
				name := protocol.name + "/" + destination.name
				if fragment {
					name += "/first fragment"
				}
				t.Run(name, func(t *testing.T) {
					// A first fragment carries a multiple of eight bytes and
					// includes the transport ports needed for admission.
					const payloadLen = 24
					frame := make([]byte, header.EthernetMinimumSize+header.IPv4MinimumSize+payloadLen)
					header.Ethernet(frame).Encode(&header.EthernetFields{
						SrcAddr: tcpip.LinkAddress(p.mac), DstAddr: destination.mac, Type: header.IPv4ProtocolNumber,
					})
					ip := header.IPv4(frame[header.EthernetMinimumSize:])
					fields := header.IPv4Fields{
						TotalLength: header.IPv4MinimumSize + payloadLen,
						TTL:         64, Protocol: uint8(protocol.number), SrcAddr: p.addr4, DstAddr: addr4(destination.addr),
					}
					if fragment {
						fields.Flags = header.IPv4FlagMoreFragments
					}
					ip.Encode(&fields)
					ip.SetChecksum(^ip.CalculateChecksum())
					if protocol.number == header.UDPProtocolNumber {
						header.UDP(ip.Payload()).Encode(&header.UDPFields{SrcPort: 12345, DstPort: dnsPort, Length: payloadLen})
					} else {
						header.TCP(ip.Payload()).Encode(&header.TCPFields{SrcPort: 12345, DstPort: dnsPort, DataOffset: header.TCPMinimumSize, Flags: header.TCPFlagSyn})
					}
					before := p.rejected.Load()
					n.fromGuest(p, frame)
					want := uint64(1)
					if destination.addr == n.gateway && !fragment {
						want = 0
					}
					if got := p.rejected.Load() - before; got != want {
						t.Fatalf("rejected frames = %d, want %d", got, want)
					}
				})
			}
		}
	}
}

func TestDNSRepliesStayWithTheirPort(t *testing.T) {
	addr := netip.MustParseAddr("192.168.127.2")
	n := &Network{gateway4: addr4(netip.MustParseAddr("192.168.127.1")), byAddr: make(map[netip.Addr]*port)}
	old := &port{n: n, addr: addr, out: make(chan []byte, 1)}
	n.byAddr[addr] = old
	ep := &networkEndpoint{Endpoint: channel.New(4, DefaultMTU, ""), n: n}
	defer ep.Close()
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
		ReserveHeaderBytes: header.EthernetMinimumSize + header.IPv4MinimumSize + header.UDPMinimumSize,
		Payload:            buffer.MakeWithData([]byte("reply")),
	})
	defer pkt.DecRef()
	pkt.NetworkProtocolNumber, pkt.TransportProtocolNumber = header.IPv4ProtocolNumber, header.UDPProtocolNumber
	header.UDP(pkt.TransportHeader().Push(header.UDPMinimumSize)).Encode(&header.UDPFields{SrcPort: dnsPort, DstPort: 12345, Length: header.UDPMinimumSize + 5})
	header.IPv4(pkt.NetworkHeader().Push(header.IPv4MinimumSize)).Encode(&header.IPv4Fields{
		TotalLength: header.IPv4MinimumSize + header.UDPMinimumSize + 5,
		Protocol:    uint8(header.UDPProtocolNumber), SrcAddr: n.gateway4, DstAddr: addr4(addr),
	})
	pkt.LinkHeader().Push(header.EthernetMinimumSize)
	var packets stack.PacketBufferList
	packets.PushBack(pkt)
	if count, err := ep.WritePackets(packets); err != nil || count != 1 {
		t.Fatalf("write DNS reply = %d, %v", count, err)
	}
	if ep.NumQueued() != 0 {
		t.Fatal("DNS reply entered the shared output queue")
	}
	next := &port{n: n, addr: addr, out: make(chan []byte, 1)}
	n.byAddr[addr] = next
	if len(old.out) != 1 || len(next.out) != 0 {
		t.Fatal("queued reply changed attachment when the address was reused")
	}
	if frame := <-old.out; !bytes.HasSuffix(frame, []byte("reply")) {
		t.Fatalf("queued reply lost its payload: %x", frame)
	}
}

func TestGatewayRejectsTrailingDNSFragments(t *testing.T) {
	n := newTestNetwork(t, Config{Egress: vmnet.DenyAll{}})
	attached, err := n.Attach(t.Context(), idleLink(t, n.MTU()), vmnet.AttachOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p := attached.(*port)
	for _, protocol := range []tcpip.TransportProtocolNumber{header.TCPProtocolNumber, header.UDPProtocolNumber} {
		frame := make([]byte, header.EthernetMinimumSize+header.IPv4MinimumSize+8)
		header.Ethernet(frame).Encode(&header.EthernetFields{SrcAddr: tcpip.LinkAddress(p.mac), DstAddr: tcpip.LinkAddress(n.gatewayHW), Type: header.IPv4ProtocolNumber})
		ip := header.IPv4(frame[header.EthernetMinimumSize:])
		ip.Encode(&header.IPv4Fields{
			TotalLength: header.IPv4MinimumSize + 8, FragmentOffset: 8,
			TTL: 64, Protocol: uint8(protocol), SrcAddr: p.addr4, DstAddr: n.gateway4,
		})
		ip.SetChecksum(^ip.CalculateChecksum())
		before := p.rejected.Load()
		n.fromGuest(p, frame)
		if p.rejected.Load() != before+1 {
			t.Fatal("gateway accepted a fragment without transport ports")
		}
	}
}
