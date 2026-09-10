package userspace

import (
	"math"
	"net"
	"strings"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

const (
	dhcpServerPort = 67
	dhcpClientPort = 68
)

// leaseTime is infinite: a port's address never changes while it is
// attached, and a client that renews gets the same answer.
var leaseTime = time.Duration(math.MaxUint32) * time.Second

// dhcpServer answers guests with the static lease their port holds. It is
// the only way a guest learns its address, gateway, and resolver.
type dhcpServer struct {
	n    *Network
	conn *gonet.UDPConn
}

func (n *Network) startDHCP() error {
	var wq waiter.Queue
	ep, err := n.stack.NewEndpoint(udp.ProtocolNumber, ipv4.ProtocolNumber, &wq)
	if err != nil {
		return tcpipError("dhcp endpoint", err)
	}
	ep.SocketOptions().SetBroadcast(true)
	if err := ep.Bind(tcpip.FullAddress{NIC: nicID, Port: dhcpServerPort}); err != nil {
		ep.Close()
		return tcpipError("dhcp bind", err)
	}
	s := &dhcpServer{n: n, conn: gonet.NewUDPConn(&wq, ep)}
	n.dhcp = s
	n.wg.Add(1)
	go s.serve()
	return nil
}

func (s *dhcpServer) serve() {
	defer s.n.wg.Done()
	buf := make([]byte, maxMTU)
	for {
		k, peer, err := s.conn.ReadFrom(buf)
		if err != nil {
			return
		}
		req, err := dhcpv4.FromBytes(buf[:k])
		if err != nil || req.OpCode != dhcpv4.OpcodeBootRequest {
			continue
		}
		reply := s.reply(req)
		if reply == nil {
			continue
		}
		// A client without an address, or one that asks for it, hears the
		// answer on the broadcast address.
		dst := peer
		if u, ok := peer.(*net.UDPAddr); !ok || u.IP == nil || u.IP.IsUnspecified() || req.IsBroadcast() {
			dst = &net.UDPAddr{IP: net.IPv4bcast, Port: dhcpClientPort}
		}
		if _, err := s.conn.WriteTo(reply.ToBytes(), dst); err != nil {
			s.n.logger.Debug("dhcp reply failed", "mac", req.ClientHWAddr, "err", err)
		}
	}
}

// reply builds the answer to a request, or nil to ignore it.
func (s *dhcpServer) reply(req *dhcpv4.DHCPv4) *dhcpv4.DHCPv4 {
	n := s.n
	n.mu.Lock()
	p := n.byMAC[req.ClientHWAddr.String()]
	n.mu.Unlock()
	if p == nil {
		n.logger.Debug("dhcp request from an unknown MAC", "mac", req.ClientHWAddr)
		return nil
	}
	reply, err := dhcpv4.NewReplyFromRequest(req)
	if err != nil {
		return nil
	}
	gateway := n.gateway.AsSlice()
	reply.ServerIPAddr = gateway
	reply.UpdateOption(dhcpv4.OptServerIdentifier(gateway))

	addr := p.addr.AsSlice()
	switch req.MessageType() {
	case dhcpv4.MessageTypeDiscover:
		reply.UpdateOption(dhcpv4.OptMessageType(dhcpv4.MessageTypeOffer))
	case dhcpv4.MessageTypeRequest:
		want := req.RequestedIPAddress()
		if want == nil {
			want = req.ClientIPAddr
		}
		if want != nil && !want.IsUnspecified() && !want.Equal(addr) {
			// The client remembers a lease from elsewhere; a NAK sends it
			// back to discovery.
			reply.UpdateOption(dhcpv4.OptMessageType(dhcpv4.MessageTypeNak))
			reply.UpdateOption(dhcpv4.OptMessage("not this client's lease"))
			return reply
		}
		reply.UpdateOption(dhcpv4.OptMessageType(dhcpv4.MessageTypeAck))
	case dhcpv4.MessageTypeInform:
		// The client configured its address itself and wants the rest.
		reply.UpdateOption(dhcpv4.OptMessageType(dhcpv4.MessageTypeAck))
		s.configure(reply, p)
		return reply
	default:
		return nil
	}
	reply.YourIPAddr = addr
	reply.UpdateOption(dhcpv4.OptIPAddressLeaseTime(leaseTime))
	s.configure(reply, p)
	return reply
}

// configure adds the network parameters every reply carries.
func (s *dhcpServer) configure(reply *dhcpv4.DHCPv4, p *port) {
	n := s.n
	gateway := n.gateway.AsSlice()
	reply.UpdateOption(dhcpv4.OptSubnetMask(net.CIDRMask(n.subnet.Bits(), 32)))
	reply.UpdateOption(dhcpv4.OptRouter(gateway))
	reply.UpdateOption(dhcpv4.OptDNS(gateway))
	reply.UpdateOption(dhcpv4.OptBroadcastAddress(n.broadcast.AsSlice()))
	reply.UpdateOption(dhcpv4.OptGeneric(dhcpv4.OptionInterfaceMTU, dhcpv4.Uint16(n.mtu).ToBytes()))
	if name := hostnameLabel(p.name); name != "" {
		reply.UpdateOption(dhcpv4.OptHostName(name))
	}
}

// hostnameLabel returns the machine name as a DNS label a guest can adopt
// as its hostname, or "" when it is not one.
func hostnameLabel(name string) string {
	name = strings.ToLower(name)
	if len(name) == 0 || len(name) > 63 || name[0] == '-' || name[len(name)-1] == '-' {
		return ""
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return ""
		}
	}
	return name
}
