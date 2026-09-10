// Package egress is the standard vmnet.Egress: an allowlist of destinations
// by name pattern or address range, a deny list of address ranges checked
// on what names resolve to, per-guest narrowing from vm.Egress, and a
// record of every decision. A rule can also inspect: the flow's TLS and
// HTTP are terminated with a certificate minted from the Policy's CA, each
// request is recorded, and secret tokens the guest holds are replaced by
// the real values on the way out, so a guest uses a credential it never
// sees.
//
// Name rules need a network that tells the Egress which name a guest
// resolved (userspace.DNSFakeIP); on a network that resolves names itself
// only address rules can match.
package egress

import (
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/shazow/virtle/vm"
	"github.com/shazow/virtle/vmnet"
)

// Rule allows flows to a set of destinations.
type Rule struct {
	// Hosts are destination patterns: a name pattern in path.Match syntax
	// ("*.github.com" matches every name under github.com, not github.com
	// itself), a CIDR, or a single address. Name patterns match a flow's
	// Host, address patterns a flow dialed by address.
	Hosts []string
	// Ports the rule applies to; empty means any.
	Ports []int
	// Inspect terminates the flow's TLS (with a certificate minted from the
	// Policy's CA, which the guest must trust) and HTTP, records each
	// request, and replaces secret tokens on the way out. Without it the
	// flow is spliced to the destination untouched. Only TCP is inspected;
	// a UDP flow to a host an inspecting rule matches is refused.
	Inspect bool
}

// Decision is what a Policy did with a flow.
type Decision string

const (
	Allowed Decision = "allow"
	Denied  Decision = "deny"
)

// Event records one decision.
type Event struct {
	Time     time.Time
	Guest    string
	Proto    vm.Proto
	Src, Dst netip.AddrPort
	Host     string         // the name the guest resolved, when known
	Decision Decision       // Allowed or Denied
	Rule     string         // the pattern that allowed the flow
	Reason   string         // why the flow was denied
	Upstream netip.AddrPort // where an allowed flow was dialed
	Err      error          // why an allowed flow's dial or request failed

	// Inspected flows also record one Event per HTTP request.
	Method string
	Path   string
	Status int // the upstream's status; 403 when refused, 502 when the upstream could not be reached
	// Injections are the Injections whose tokens the request carried, by
	// name, or by token when unnamed.
	Injections []string
}

// Recorder receives every Event.
type Recorder interface{ Record(Event) }

// Resolver resolves the names of allowed flows; *net.Resolver implements it.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// DefaultDenyPrefixes are the ranges a Policy with nil DenyPrefixes refuses:
// the host's own loopback, link-local (cloud metadata services live there),
// unspecified, multicast, and broadcast addresses.
var DefaultDenyPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("255.255.255.255/32"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
	netip.MustParsePrefix("fd00:ec2::254/128"),
}

// Policy is a vmnet.Egress that allows what its Rules name, denies
// everything else, and records each decision. The zero value denies every
// flow. It is safe for concurrent use once configured.
type Policy struct {
	// Rules allow flows; a flow no rule matches is denied.
	Rules []Rule
	// DenyPrefixes are address ranges no flow may reach whatever the Rules
	// say, checked on the addresses a name resolves to as well as on
	// addresses guests dial directly. Nil means DefaultDenyPrefixes; an
	// empty, non-nil slice denies no range.
	DenyPrefixes []netip.Prefix
	// Recorder receives one Event per decision. Nil records through Logger.
	Recorder Recorder
	// Logger is where the default Recorder writes; nil discards.
	Logger *slog.Logger
	// Dialer dials allowed flows; nil means a zero Dialer.
	Dialer *net.Dialer
	// Resolver resolves the names of allowed flows; nil means
	// net.DefaultResolver.
	Resolver Resolver

	// Injections are tokens inspected requests carry in place of a value
	// computed as they pass, or that refuse them; named ones are secrets
	// issued to guests. See Injection.
	Injections []Injection
	// Admit is consulted for every inspected request before any token is
	// replaced, with the request as the guest sent it. An error wrapping
	// vmnet.ErrDenied refuses the request (the guest gets 403), any other
	// error fails it (502); either is on record. Nil admits every request.
	Admit func(ctx context.Context, req Request) error
	// CA signs the certificates inspected flows present to guests; see
	// LoadOrCreateCA. Required by any Rule with Inspect.
	CA tls.Certificate
	// UpstreamTLS configures TLS from an inspected flow to its real
	// destination; nil verifies against the system roots.
	UpstreamTLS *tls.Config

	mu     sync.Mutex
	leaves map[string]*tls.Certificate // minted per host
	tokens map[string]string           // generated per Injection.Name
}

// DialFlow implements vmnet.Egress. A denied flow fails with an error
// wrapping vmnet.ErrDenied, so the network refuses it before the guest sees
// it accepted; a name that resolves only to denied ranges counts as denied.
func (p *Policy) DialFlow(ctx context.Context, f vmnet.Flow) (net.Conn, error) {
	ev := Event{Time: time.Now(), Guest: f.Guest, Proto: cmp.Or(f.Proto, vm.TCP), Src: f.Src, Dst: f.Dst, Host: f.Host}
	matched, rule, reason := p.decide(f)
	if reason == "" && matched.Inspect && f.Network() != "tcp" {
		// Inspection terminates TCP; a datagram flow to the same host
		// would pass unseen, so it is refused and the guest falls back.
		reason = "only tcp is inspected"
	}
	if reason != "" {
		ev.Decision, ev.Reason = Denied, reason
		p.record(ev)
		return nil, fmt.Errorf("egress: %s: %w", reason, vmnet.ErrDenied)
	}
	if matched.Inspect {
		// The dial happens per request, inside the proxy; the flow itself
		// is recorded as allowed now and each request as it is made.
		conn, err := p.inspect(ctx, f, rule)
		ev.Rule = rule
		if err != nil {
			ev.Decision, ev.Reason = Denied, err.Error()
		} else {
			ev.Decision = Allowed
		}
		p.record(ev)
		return conn, err
	}
	conn, upstream, err := p.dial(ctx, f)
	ev.Rule, ev.Upstream = rule, upstream
	switch {
	case errors.Is(err, vmnet.ErrDenied):
		ev.Decision, ev.Reason = Denied, err.Error()
	case err != nil:
		ev.Decision, ev.Err = Allowed, err
	default:
		ev.Decision = Allowed
	}
	p.record(ev)
	return conn, err
}

// decide applies the guest's own policy, the deny ranges for a flow dialed
// by address, and the Rules. It returns the allowing rule and pattern, or
// the reason for a denial.
func (p *Policy) decide(f vmnet.Flow) (matched Rule, pattern, reason string) {
	if f.Egress != nil {
		for _, r := range f.Egress.Deny {
			if matchPattern(r.Host, r.Ports, f) {
				return Rule{}, "", "denied by the guest's policy"
			}
		}
		allowed := false
		for _, r := range f.Egress.Allow {
			if matchPattern(r.Host, r.Ports, f) {
				allowed = true
				break
			}
		}
		if !allowed {
			return Rule{}, "", "outside the guest's allow list"
		}
	}
	if f.Host == "" && p.denied(f.Dst.Addr()) {
		return Rule{}, "", fmt.Sprintf("%s is in a denied range", f.Dst.Addr())
	}
	for _, r := range p.Rules {
		for _, h := range r.Hosts {
			if matchPattern(h, r.Ports, f) {
				return r, h, ""
			}
		}
	}
	return Rule{}, "", "no rule allows it"
}

// dial connects an allowed flow: by address when that is all the guest
// gave, else by resolving the name and refusing addresses in denied ranges.
func (p *Policy) dial(ctx context.Context, f vmnet.Flow) (net.Conn, netip.AddrPort, error) {
	dialer := p.Dialer
	if dialer == nil {
		dialer = &net.Dialer{}
	}
	if f.Host == "" {
		conn, err := dialer.DialContext(ctx, f.Network(), f.Dst.String())
		return conn, f.Dst, err
	}
	resolver := p.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	host := normalizeName(f.Host)
	addrs, err := resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, netip.AddrPort{}, fmt.Errorf("resolve %s: %w", host, err)
	}
	var firstErr error
	for _, a := range addrs {
		a = a.Unmap()
		if p.denied(a) {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s resolves to %s, in a denied range: %w", host, a, vmnet.ErrDenied)
			}
			continue
		}
		upstream := netip.AddrPortFrom(a, f.Dst.Port())
		conn, err := dialer.DialContext(ctx, f.Network(), upstream.String())
		if err == nil {
			return conn, upstream, nil
		}
		if firstErr == nil || errors.Is(firstErr, vmnet.ErrDenied) {
			firstErr = err
		}
	}
	if firstErr == nil {
		firstErr = fmt.Errorf("%s resolves to no address", host)
	}
	return nil, netip.AddrPort{}, firstErr
}

func (p *Policy) denied(a netip.Addr) bool {
	prefixes := p.DenyPrefixes
	if prefixes == nil {
		prefixes = DefaultDenyPrefixes
	}
	a = a.Unmap()
	for _, prefix := range prefixes {
		if prefix.Contains(a) {
			return true
		}
	}
	return false
}

func (p *Policy) record(ev Event) {
	if p.Recorder != nil {
		p.Recorder.Record(ev)
		return
	}
	if p.Logger == nil {
		return
	}
	attrs := []any{
		"decision", ev.Decision, "guest", ev.Guest, "proto", ev.Proto,
		"src", ev.Src, "dst", ev.Dst,
	}
	if ev.Host != "" {
		attrs = append(attrs, "host", ev.Host)
	}
	if ev.Rule != "" {
		attrs = append(attrs, "rule", ev.Rule)
	}
	if ev.Reason != "" {
		attrs = append(attrs, "reason", ev.Reason)
	}
	if ev.Upstream.IsValid() {
		attrs = append(attrs, "upstream", ev.Upstream)
	}
	if ev.Method != "" {
		attrs = append(attrs, "method", ev.Method, "path", ev.Path, "status", ev.Status)
	}
	if len(ev.Injections) != 0 {
		attrs = append(attrs, "injections", ev.Injections)
	}
	if ev.Err != nil {
		attrs = append(attrs, "err", ev.Err)
	}
	p.Logger.Info("egress flow", attrs...)
}

// matchPattern reports whether a host pattern with optional ports matches
// the flow: an address or CIDR against a flow dialed by address, a name
// pattern against the name the guest resolved.
func matchPattern(pattern string, ports []int, f vmnet.Flow) bool {
	if len(ports) != 0 && !slices.Contains(ports, int(f.Dst.Port())) {
		return false
	}
	if prefix, err := netip.ParsePrefix(pattern); err == nil {
		return f.Host == "" && prefix.Contains(f.Dst.Addr().Unmap())
	}
	if addr, err := netip.ParseAddr(pattern); err == nil {
		return f.Host == "" && addr.Unmap() == f.Dst.Addr().Unmap()
	}
	if f.Host == "" {
		return false
	}
	ok, err := path.Match(normalizeName(pattern), normalizeName(f.Host))
	return err == nil && ok
}

func normalizeName(name string) string {
	return strings.ToLower(strings.TrimSuffix(name, "."))
}

// ValidPattern reports whether a Rule or vm.Reach host pattern is
// well-formed: an address, a CIDR, or a name pattern path.Match accepts.
func ValidPattern(pattern string) error {
	if pattern == "" {
		return errors.New("empty host pattern")
	}
	if _, err := netip.ParsePrefix(pattern); err == nil {
		return nil
	}
	if _, err := netip.ParseAddr(pattern); err == nil {
		return nil
	}
	if _, err := path.Match(pattern, ""); err != nil {
		return fmt.Errorf("host pattern %q: %w", pattern, err)
	}
	if strings.ContainsAny(pattern, "/: ") {
		return fmt.Errorf("host pattern %q is neither a name pattern nor an address", pattern)
	}
	return nil
}

var _ vmnet.Egress = (*Policy)(nil)
