package egress

import (
	"context"
	"fmt"

	"github.com/shazow/virtle/vmnet"
)

// AuthorizeDNS admits a DNS query by name, before any lookup. Hostname
// allows apply regardless of their service ports, since a DNS query names
// no service. Guest hostname denies without a port restriction win; address
// rules and port-specific denies are enforced when a flow is dialed.
// Every record type uses the same name policy.
func (p *Policy) AuthorizeDNS(_ context.Context, f vmnet.Flow, _ uint16) error {
	deny := func(reason string) error {
		return fmt.Errorf("egress DNS: %s: %w", reason, vmnet.ErrDenied)
	}
	if f.Host == "" {
		return deny("no question name")
	}
	if f.Egress != nil {
		for _, r := range f.Egress.Deny {
			if len(r.Ports) == 0 && matchPattern(r.Host, nil, f) {
				return deny("denied by the guest's policy")
			}
		}
		allowed := false
		for _, r := range f.Egress.Allow {
			if matchPattern(r.Host, nil, f) {
				allowed = true
				break
			}
		}
		if !allowed {
			return deny("outside the guest's allow list")
		}
	}
	for _, r := range p.Rules {
		for _, host := range r.Hosts {
			if matchPattern(host, nil, f) {
				return nil
			}
		}
	}
	if p.Reach == ReachInternet || p.Reach == ReachAll {
		return nil
	}
	return deny("no rule allows the name")
}

var _ vmnet.DNSAuthorizer = (*Policy)(nil)
