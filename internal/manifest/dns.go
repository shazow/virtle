package manifest

import (
	"fmt"
	"net/netip"
	"strings"
)

// resolveDNSUpstream validates configuration without consulting host resolver
// settings or opening sockets. The network resolves "host" when it is built.
func resolveDNSUpstream(in *DNSInput) (string, error) {
	upstream := ""
	if in != nil {
		upstream = strings.TrimSpace(in.Upstream)
	}
	if upstream == "" || upstream == "host" {
		return "host", nil
	}
	addr, err := netip.ParseAddrPort(upstream)
	if err != nil {
		return "", fmt.Errorf("must be host or an IP address with a port: %w", err)
	}
	if addr.Port() == 0 {
		return "", fmt.Errorf("port must be between 1 and 65535")
	}
	return addr.String(), nil
}
