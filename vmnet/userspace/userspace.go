// Package userspace is an in-process guest network on gVisor's netstack.
// Links attach to one Ethernet segment whose gateway serves DHCP and DNS,
// switches frames between guests, terminates guest TCP and UDP flows in
// userspace, and dials each one through the network's Egress. It touches no
// host network configuration and needs no privilege; guest-to-guest traffic
// never leaves the process.
//
// The segment carries IPv4 only. The gateway answers ICMP echo, but nothing
// forwards ICMP: a ping to the outside world goes unanswered.
package userspace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/ethernet"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"

	"github.com/shazow/virtle/internal/dnsproxy"
	"github.com/shazow/virtle/vmnet"
)

// DefaultMTU is the segment MTU when Config.MTU is zero.
const DefaultMTU = 1500

// DefaultSubnet is the guest network when Config.Subnet is zero. It is the
// range gvisor-tap-vsock and podman machine use, so it rarely collides with
// a host's own networks.
var DefaultSubnet = netip.MustParsePrefix("192.168.127.0/24")

const (
	nicID tcpip.NICID = 1

	// minMTU is the smallest IPv4 datagram every host must accept; DHCP
	// messages rely on it.
	minMTU = 576
	// maxMTU is the largest IPv4 datagram.
	maxMTU = 65535

	// egressQueue bounds frames waiting for one guest's link; a guest that
	// stops reading loses frames rather than stalling the others.
	egressQueue = 256
	// stackQueue bounds frames the stack has sent and the switch has not yet
	// dispatched.
	stackQueue = 1024

	// dialTimeout bounds an Egress dial for a TCP flow; the guest keeps
	// retransmitting its SYN meanwhile.
	dialTimeout = 30 * time.Second
	// udpDialTimeout bounds an Egress dial for a UDP flow, which runs on the
	// packet path.
	udpDialTimeout = 5 * time.Second
	// udpIdleTimeout ends a UDP flow or forward peer with no traffic.
	udpIdleTimeout = 90 * time.Second
	// maxInFlight caps TCP handshakes waiting on an Egress dial.
	maxInFlight = 1024
)

// Config configures a Network. The zero value is usable.
type Config struct {
	// Subnet is the IPv4 network guests live on. Default DefaultSubnet.
	Subnet netip.Prefix
	// Gateway is the network's own address: it serves DHCP and DNS, is the
	// guests' default route, and is what Listen binds. Default: the first
	// address of Subnet.
	Gateway netip.Addr
	// MTU is the largest IP packet on the segment; a link must carry at
	// least this much to attach. Default DefaultMTU.
	MTU int
	// FakeIPRange is where synthetic DNS answers come from; it must not overlap
	// Subnet. Default DefaultFakeIPRange.
	FakeIPRange netip.Prefix
	// DNSUpstream is the resolver used for DNS and outbound name resolution:
	// "host" (the default) or an IP:port. Guests still receive synthetic A
	// addresses. Remote queries require Egress to implement vmnet.DNSAuthorizer.
	DNSUpstream string
	// Egress dials guest-initiated flows. Default vmnet.Passthrough{}.
	Egress vmnet.Egress
	// Logger receives attach, flow, DNS, and drop events; nil discards them.
	Logger *slog.Logger
}

// Network implements vmnet.Network: one stack shared by every attached
// port, so guests on it reach each other, each with a fixed address and a
// static DHCP lease keyed by its MAC. Close it when no machine needs it.
type Network struct {
	subnet    netip.Prefix
	gateway   netip.Addr
	gateway4  tcpip.Address
	gatewayHW net.HardwareAddr
	broadcast netip.Addr
	mtu       int
	egress    vmnet.Egress
	logger    *slog.Logger

	stack       *stack.Stack
	ep          *channel.Endpoint
	ctx         context.Context
	cancel      context.CancelFunc
	wg          sync.WaitGroup
	dhcp        *dhcpServer
	dns         *dnsServer
	fakeIPs     *fakeIPTable
	resolver    *dnsproxy.Resolver
	dnsUpstream string

	mu     sync.Mutex
	closed bool
	byAddr map[netip.Addr]*port
	byMAC  map[string]*port
	cursor netip.Addr // the last allocated address
}

// New creates a network and starts its gateway services.
func New(cfg Config) (*Network, error) {
	n := &Network{
		subnet: cfg.Subnet,
		mtu:    cfg.MTU,
		egress: cfg.Egress,
		logger: cfg.Logger,
		byAddr: make(map[netip.Addr]*port),
		byMAC:  make(map[string]*port),
	}
	if !n.subnet.IsValid() {
		n.subnet = DefaultSubnet
	}
	n.subnet = n.subnet.Masked()
	if !n.subnet.Addr().Is4() {
		return nil, fmt.Errorf("userspace: subnet %s is not IPv4", cfg.Subnet)
	}
	if n.subnet.Bits() > 30 {
		return nil, fmt.Errorf("userspace: subnet %s has no room for a gateway and a guest", n.subnet)
	}
	n.broadcast = lastAddr(n.subnet)
	n.gateway = cfg.Gateway
	if !n.gateway.IsValid() {
		n.gateway = n.subnet.Addr().Next()
	}
	n.gateway = n.gateway.Unmap()
	if !n.subnet.Contains(n.gateway) || n.gateway == n.subnet.Addr() || n.gateway == n.broadcast {
		return nil, fmt.Errorf("userspace: gateway %s is not a host address in %s", n.gateway, n.subnet)
	}
	n.gateway4 = addr4(n.gateway)
	n.gatewayHW = macFor(n.gateway)
	if n.mtu == 0 {
		n.mtu = DefaultMTU
	}
	if n.mtu < minMTU || n.mtu > maxMTU {
		return nil, fmt.Errorf("userspace: MTU %d is outside %d-%d", n.mtu, minMTU, maxMTU)
	}
	fakeRange := cfg.FakeIPRange
	if !fakeRange.IsValid() {
		fakeRange = DefaultFakeIPRange
	}
	fakeRange = fakeRange.Masked()
	switch {
	case !fakeRange.Addr().Is4() || fakeRange.Bits() > 30:
		return nil, fmt.Errorf("userspace: fake IP range %s is not an IPv4 range with room for names", cfg.FakeIPRange)
	case fakeRange.Overlaps(n.subnet):
		return nil, fmt.Errorf("userspace: fake IP range %s overlaps the subnet %s", fakeRange, n.subnet)
	}
	n.fakeIPs = newFakeIPTable(fakeRange)
	if n.egress == nil {
		n.egress = vmnet.Passthrough{}
	}
	if n.logger == nil {
		n.logger = slog.New(slog.DiscardHandler)
	}
	var err error
	n.resolver, err = dnsproxy.New(cfg.DNSUpstream)
	if err != nil {
		return nil, fmt.Errorf("userspace: DNS upstream: %w", err)
	}
	n.dnsUpstream = strings.TrimSpace(cfg.DNSUpstream)
	if n.dnsUpstream == "" {
		n.dnsUpstream = "host"
	}

	n.ep = channel.New(stackQueue, uint32(n.mtu+header.EthernetMinimumSize), tcpip.LinkAddress(n.gatewayHW))
	// Frames from a guest crossed a reliable stream, and a guest whose NIC
	// offloads checksums may not have filled them in.
	n.ep.LinkEPCapabilities = stack.CapabilityRXChecksumOffload
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol4},
	})
	n.stack = s
	n.ctx, n.cancel = context.WithCancel(context.Background())
	if err := n.configureStack(); err != nil {
		n.cancel()
		s.Destroy()
		return nil, err
	}
	n.installForwarders()
	if err := n.startDHCP(); err != nil {
		_ = n.Close()
		return nil, err
	}
	if err := n.startDNS(); err != nil {
		_ = n.Close()
		return nil, err
	}
	n.wg.Add(1)
	go n.runSwitch()
	return n, nil
}

func (n *Network) configureStack() error {
	s := n.stack
	if err := s.CreateNIC(nicID, ethernet.New(&networkEndpoint{Endpoint: n.ep, n: n})); err != nil {
		return tcpipError("create NIC", err)
	}
	if err := s.AddProtocolAddress(nicID, tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{Address: n.gateway4, PrefixLen: n.subnet.Bits()},
	}, stack.AddressProperties{}); err != nil {
		return tcpipError("add gateway address", err)
	}
	// Promiscuous: accept packets for any destination, so guest flows to the
	// outside reach the forwarders. Spoofing: answer them from that
	// destination's address.
	if err := s.SetPromiscuousMode(nicID, true); err != nil {
		return tcpipError("set promiscuous mode", err)
	}
	if err := s.SetSpoofing(nicID, true); err != nil {
		return tcpipError("set spoofing", err)
	}
	sub, err := tcpip.NewSubnet(addr4(n.subnet.Addr()), tcpip.MaskFromBytes(net.CIDRMask(n.subnet.Bits(), 32)))
	if err != nil {
		return fmt.Errorf("userspace: subnet route: %w", err)
	}
	s.SetRouteTable([]tcpip.Route{{Destination: sub, NIC: nicID}})
	return nil
}

// Gateway is the network's own address.
func (n *Network) Gateway() netip.Addr { return n.gateway }

// Subnet is the network guests live on.
func (n *Network) Subnet() netip.Prefix { return n.subnet }

// MTU is the segment's MTU.
func (n *Network) MTU() int { return n.mtu }

// Attach implements vmnet.Network.
func (n *Network) Attach(ctx context.Context, link vmnet.Link, opts vmnet.AttachOptions) (vmnet.Port, error) {
	if link.MTU() < n.mtu {
		return nil, fmt.Errorf("userspace: link MTU %d is below the network's %d", link.MTU(), n.mtu)
	}
	p, err := n.newPort(link, opts)
	if err != nil {
		return nil, err
	}
	// Install the neighbor before reading guest frames. TCP teardown relies
	// on a SYN-ACK reaching the link endpoint without waiting on ARP.
	p.mu.Lock()
	if p.closed {
		err = net.ErrClosed
	} else if terr := n.stack.AddStaticNeighbor(nicID, ipv4.ProtocolNumber, p.addr4, tcpip.LinkAddress(p.mac)); terr != nil {
		err = tcpipError("add neighbor", terr)
	}
	p.mu.Unlock()
	// newPort counts both pumps; start them even if attachment failed so
	// Network.Close has no missing goroutines to wait for.
	go n.portRx(p)
	go n.portTx(p)
	if err != nil {
		_ = p.Close()
		return nil, err
	}
	n.logger.Info("network port attached", "guest", p.name, "addr", p.addr, "mac", p.mac.String())
	return p, nil
}

// newPort reserves an address and a MAC for the link, and counts the two
// pumps Attach starts for it.
func (n *Network) newPort(link vmnet.Link, opts vmnet.AttachOptions) (*port, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return nil, net.ErrClosed
	}
	addr := opts.Addr.Unmap()
	if addr.IsValid() {
		switch {
		case !n.subnet.Contains(addr) || addr == n.subnet.Addr() || addr == n.broadcast:
			return nil, fmt.Errorf("userspace: address %s is not a host address in %s", addr, n.subnet)
		case addr == n.gateway:
			return nil, fmt.Errorf("userspace: address %s is the gateway", addr)
		}
		if _, used := n.byAddr[addr]; used {
			return nil, fmt.Errorf("userspace: address %s is in use", addr)
		}
	} else {
		var err error
		if addr, err = n.allocateLocked(); err != nil {
			return nil, err
		}
	}
	mac := opts.MAC
	if mac == nil {
		mac = macFor(addr)
	} else if len(mac) != 6 || mac[0]&1 != 0 {
		return nil, fmt.Errorf("userspace: %s is not a unicast Ethernet address", mac)
	}
	if _, used := n.byMAC[mac.String()]; used || mac.String() == n.gatewayHW.String() {
		return nil, fmt.Errorf("userspace: MAC %s is in use", mac)
	}
	ctx, cancel := context.WithCancel(n.ctx)
	p := &port{
		n:         n,
		ctx:       ctx,
		cancel:    cancel,
		attached:  n.stack.Clock().Now(),
		flows:     make(map[*forwardedFlow]struct{}),
		name:      opts.Name,
		egress:    opts.Egress,
		addr:      addr,
		addr4:     addr4(addr),
		mac:       append(net.HardwareAddr(nil), mac...),
		link:      link,
		out:       make(chan []byte, egressQueue),
		done:      make(chan struct{}),
		exposures: make(map[forwardKey]*exposure),
	}
	p.dnsTCP = tcp.NewForwarder(n.stack, 0, maxInFlight, func(r *tcp.ForwarderRequest) { n.dns.acceptTCP(p, r) })
	n.byAddr[addr] = p
	n.byMAC[mac.String()] = p
	n.wg.Add(2)
	return p, nil
}

// allocateLocked picks the next free host address after the last one
// handed out, so a released address rests before it is reused.
func (n *Network) allocateLocked() (netip.Addr, error) {
	start := n.cursor
	if !start.IsValid() {
		start = n.gateway
	}
	for a := start; ; {
		a = a.Next()
		if !n.subnet.Contains(a) || a == n.broadcast {
			a = n.subnet.Addr().Next()
		}
		if a == n.gateway {
			if a == start {
				break
			}
			continue
		}
		if _, used := n.byAddr[a]; !used {
			n.cursor = a
			return a, nil
		}
		if a == start {
			break
		}
	}
	return netip.Addr{}, fmt.Errorf("userspace: no free address in %s", n.subnet)
}

// forget releases a port's address and MAC.
func (n *Network) forget(p *port) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.byAddr[p.addr] == p {
		delete(n.byAddr, p.addr)
	}
	if n.byMAC[p.mac.String()] == p {
		delete(n.byMAC, p.mac.String())
	}
}

// portByAddr returns the attached port with the address, if any.
func (n *Network) portByAddr(a netip.Addr) *port {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.byAddr[a]
}

// portByName returns an attached port by its AttachOptions.Name.
func (n *Network) portByName(name string) *port {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, p := range n.byAddr {
		if p.name == name {
			return p
		}
	}
	return nil
}

// DialContext dials a guest from the host without a forward: network is
// "tcp" or "udp", and addr is a guest address or an attached machine's
// name, with a port.
func (n *Network) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	full, err := n.resolveGuest(network, addr)
	if err != nil {
		return nil, err
	}
	switch network {
	case "tcp", "tcp4":
		return gonet.DialContextTCP(ctx, n.stack, full, ipv4.ProtocolNumber)
	case "udp", "udp4":
		return gonet.DialUDP(n.stack, nil, &full, ipv4.ProtocolNumber)
	}
	return nil, fmt.Errorf("userspace: dial %s: %w", network, net.UnknownNetworkError(network))
}

// resolveGuest turns "addr:port" or "name:port" into a stack address.
func (n *Network) resolveGuest(network, addr string) (tcpip.FullAddress, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return tcpip.FullAddress{}, fmt.Errorf("userspace: dial %s: %w", addr, err)
	}
	port, err := net.LookupPort(network, portStr)
	if err != nil {
		return tcpip.FullAddress{}, fmt.Errorf("userspace: dial %s: %w", addr, err)
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		p := n.portByName(host)
		if p == nil {
			return tcpip.FullAddress{}, fmt.Errorf("userspace: dial %s: no attached machine named %q", addr, host)
		}
		a = p.addr
	}
	a = a.Unmap()
	if !n.subnet.Contains(a) {
		return tcpip.FullAddress{}, fmt.Errorf("userspace: dial %s: %s is outside %s", addr, a, n.subnet)
	}
	return tcpip.FullAddress{NIC: nicID, Addr: addr4(a), Port: uint16(port)}, nil
}

// Listen serves TCP on the gateway address from the host, for a service
// guests reach without leaving the network. addr is ":port" or the gateway
// address with a port; network must be "tcp".
func (n *Network) Listen(network, addr string) (net.Listener, error) {
	switch network {
	case "tcp", "tcp4":
	default:
		return nil, fmt.Errorf("userspace: listen %s: %w", network, net.UnknownNetworkError(network))
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("userspace: listen %s: %w", addr, err)
	}
	if host != "" {
		a, err := netip.ParseAddr(host)
		if err != nil || a.Unmap() != n.gateway {
			return nil, fmt.Errorf("userspace: listen %s: only the gateway address %s can be bound", addr, n.gateway)
		}
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return nil, fmt.Errorf("userspace: listen %s: %w", addr, err)
	}
	if port == dnsPort {
		return nil, fmt.Errorf("userspace: listen %s: port is reserved for gateway DNS", addr)
	}
	return gonet.ListenTCP(n.stack, tcpip.FullAddress{NIC: nicID, Addr: n.gateway4, Port: uint16(port)}, ipv4.ProtocolNumber)
}

// Close closes every attached port and stops the gateway services. It is
// idempotent.
func (n *Network) Close() error {
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return nil
	}
	n.closed = true
	ports := make([]*port, 0, len(n.byAddr))
	for _, p := range n.byAddr {
		ports = append(ports, p)
	}
	n.mu.Unlock()
	var errs []error
	for _, p := range ports {
		if err := p.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if n.dns != nil {
		n.dns.shutdown()
	}
	if n.dhcp != nil {
		_ = n.dhcp.conn.Close()
	}
	n.cancel()
	n.stack.Destroy()
	n.wg.Wait()
	return errors.Join(errs...)
}

// tcpipError wraps a netstack error, which is not a Go error.
func tcpipError(op string, err tcpip.Error) error {
	return fmt.Errorf("userspace: %s: %s", op, err)
}

func addr4(a netip.Addr) tcpip.Address { return tcpip.AddrFrom4(a.Unmap().As4()) }

func netipAddr(a tcpip.Address) netip.Addr { return netip.AddrFrom4(a.As4()) }

// lastAddr is the subnet's broadcast address.
func lastAddr(p netip.Prefix) netip.Addr {
	b := p.Addr().As4()
	mask := net.CIDRMask(p.Bits(), 32)
	for i := range b {
		b[i] |= ^mask[i]
	}
	return netip.AddrFrom4(b)
}

// macFor derives a locally administered unicast MAC from an address, so
// every guest on a segment has a distinct one without bookkeeping.
func macFor(a netip.Addr) net.HardwareAddr {
	b := a.Unmap().As4()
	return net.HardwareAddr{0x0a, 0x56, b[0], b[1], b[2], b[3]}
}

var _ io.Closer = (*Network)(nil)
