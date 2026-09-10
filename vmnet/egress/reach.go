package egress

import (
	"net"
	"net/netip"
	"sync"
	"time"
)

// Reach is what a flow no Rule matches may reach.
type Reach string

const (
	// ReachRules allows only what the Rules name; the zero value means the
	// same.
	ReachRules Reach = "rules"
	// ReachInternet allows any public destination: an address outside
	// LocalPrefixes that is not one of the host's own, checked on what a
	// name resolves to as well as on addresses dialed directly. A flow a
	// Rule allows is not held to that, so a Rule can still name a host on
	// the LAN.
	ReachInternet Reach = "internet"
	// ReachAll allows any destination the host can reach, subject to
	// DenyPrefixes.
	ReachAll Reach = "all"
)

// LocalPrefixes are the address ranges that are not the public internet:
// private (RFC 1918), carrier-grade NAT, loopback, link-local, multicast,
// reserved, and documentation ranges, in both families. ReachInternet
// refuses them, along with the host's own addresses.
var LocalPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

// hostAddrTTL is how long the host's own addresses are trusted before they
// are read again: a VPN or a lease coming up gives the host an address a
// guest must not reach either.
const hostAddrTTL = 5 * time.Second

// hostInterfaceAddrs lists the host's interface addresses; a variable so
// tests can give the host an address of their choosing.
var hostInterfaceAddrs = net.InterfaceAddrs

// hostAddrs caches the host's own addresses.
type hostAddrs struct {
	mu    sync.Mutex
	at    time.Time
	addrs map[netip.Addr]bool
}

// has reports whether a is one of the host's own addresses.
func (h *hostAddrs) has(a netip.Addr) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.addrs == nil || time.Since(h.at) > hostAddrTTL {
		h.addrs = make(map[netip.Addr]bool)
		if addrs, err := hostInterfaceAddrs(); err == nil {
			for _, addr := range addrs {
				var ip net.IP
				switch v := addr.(type) {
				case *net.IPNet:
					ip = v.IP
				case *net.IPAddr:
					ip = v.IP
				}
				if a, ok := netip.AddrFromSlice(ip); ok {
					h.addrs[a.Unmap()] = true
				}
			}
		}
		h.at = time.Now()
	}
	return h.addrs[a.Unmap()]
}

// local reports whether an address is off the internet: in LocalPrefixes or
// one of the host's own.
func (p *Policy) local(a netip.Addr) bool {
	return inPrefixes(LocalPrefixes, a) || p.host.has(a)
}

func inPrefixes(prefixes []netip.Prefix, a netip.Addr) bool {
	a = a.Unmap()
	for _, prefix := range prefixes {
		if prefix.Contains(a) {
			return true
		}
	}
	return false
}
