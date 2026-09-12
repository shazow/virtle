// Package dnsproxy exchanges ordinary DNS questions with configured host
// nameservers. It does not cache answers or run a guest-facing server.
package dnsproxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/miekg/dns"
)

const (
	lookupTimeout = 5 * time.Second
	udpSize       = 1232
	maxAliases    = 16
)

// Resolver uses a fixed list of upstream addresses and is safe for concurrent
// use. New is required; a zero Resolver has no upstream servers.
type Resolver struct {
	servers []string
	logger  *slog.Logger
}

// WithLogger returns a resolver that reports exchanges to logger. Its
// immutable upstream configuration is shared with r.
func (r *Resolver) WithLogger(logger *slog.Logger) *Resolver {
	copy := *r
	copy.logger = logger
	return &copy
}

// New selects an explicit IP:port upstream, or the nameservers in the host's
// /etc/resolv.conf when upstream is empty or "host". An explicit upstream
// never falls back to host configuration or to a public resolver.
func New(upstream string) (*Resolver, error) {
	upstream = strings.TrimSpace(upstream)
	if upstream != "" && upstream != "host" {
		addr, err := netip.ParseAddrPort(upstream)
		if err != nil || addr.Port() == 0 {
			return nil, fmt.Errorf("dnsproxy: upstream %q must be an IP:port with a nonzero port", upstream)
		}
		return &Resolver{servers: []string{addr.String()}}, nil
	}
	f, err := os.Open("/etc/resolv.conf")
	if err != nil {
		return nil, fmt.Errorf("dnsproxy: host nameservers: %w", err)
	}
	defer f.Close()
	return readHost(f)
}

// readHost reads only nameserver addresses. DNS questions are already fully
// qualified; search suffixes and resolver-specific options do not apply.
func readHost(r io.Reader) (*Resolver, error) {
	resolver := new(Resolver)
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 || fields[0] != "nameserver" {
			continue
		}
		if len(fields) < 2 {
			return nil, fmt.Errorf("dnsproxy: nameserver is missing its address")
		}
		addr, err := netip.ParseAddr(fields[1])
		if err != nil {
			return nil, fmt.Errorf("dnsproxy: host nameserver %q: %w", fields[1], err)
		}
		server := netip.AddrPortFrom(addr, 53).String()
		if !slices.Contains(resolver.servers, server) {
			resolver.servers = append(resolver.servers, server)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("dnsproxy: read host nameservers: %w", err)
	}
	if len(resolver.servers) == 0 {
		return nil, fmt.Errorf("dnsproxy: host configuration has no nameservers")
	}
	return resolver, nil
}

// Query sends one recursive IN question over UDP, retrying over TCP when the
// reply is truncated. Host servers are tried in order on transport failures
// or SERVFAIL; other replies, including NXDOMAIN, are returned unchanged.
// The whole exchange is bounded by five seconds and the caller's context.
// It constructs its own message and EDNS options from name and qtype.
func (r *Resolver) Query(ctx context.Context, name string, qtype uint16) (response *dns.Msg, err error) {
	if name == "" {
		return nil, fmt.Errorf("dnsproxy: expected a question name")
	}
	switch qtype {
	case 0, dns.TypeOPT, dns.TypeTKEY, dns.TypeTSIG, dns.TypeIXFR, dns.TypeAXFR, dns.TypeMAILB, dns.TypeMAILA, dns.TypeANY:
		return nil, fmt.Errorf("dnsproxy: unsupported question type %d", qtype)
	}
	name = dns.CanonicalName(name)
	if _, ok := dns.IsDomainName(name); !ok {
		return nil, fmt.Errorf("dnsproxy: invalid question name %q", name)
	}
	if r == nil || len(r.servers) == 0 {
		return nil, fmt.Errorf("dnsproxy: no upstream nameservers")
	}
	query := new(dns.Msg).SetQuestion(name, qtype)
	query.SetEdns0(udpSize, false)
	started := time.Now()
	var upstream string
	defer func() {
		if r.logger != nil {
			rcode := ""
			if response != nil {
				rcode = dns.RcodeToString[response.Rcode]
			}
			r.logger.InfoContext(ctx, "dns exchange", "name", name,
				"type", qtype, "upstream", upstream, "rcode", rcode,
				"duration", time.Since(started), "err", err)
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	deadline, _ := ctx.Deadline()
	var lastErr error
	for i, server := range r.servers {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		upstream = server
		// Give each remaining server a chance within the shared deadline.
		attempt, stop := context.WithTimeout(ctx, time.Until(deadline)/time.Duration(len(r.servers)-i))
		response, err := exchange(attempt, query, server, "udp")
		if err == nil && response.Truncated {
			response, err = exchange(attempt, query, server, "tcp")
			if err == nil && response.Truncated {
				err = fmt.Errorf("dnsproxy: truncated TCP response")
			}
		}
		stop()
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = fmt.Errorf("dnsproxy: %s: %w", server, err)
			var networkErr net.Error
			if errors.As(err, &networkErr) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				continue
			}
			return nil, lastErr
		}
		if response.Rcode == dns.RcodeServerFailure && i+1 < len(r.servers) {
			continue
		}
		return response, nil
	}
	return nil, lastErr
}

func exchange(ctx context.Context, query *dns.Msg, server, network string) (*dns.Msg, error) {
	client := &dns.Client{Net: network, Timeout: lookupTimeout}
	conn, err := client.DialContext(ctx, server)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	// dns.Client honors context deadlines but not cancellation after dialing.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	response, _, err := client.ExchangeWithConnContext(ctx, query, conn)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	if !response.Response || response.Opcode != dns.OpcodeQuery || len(response.Question) != 1 {
		return nil, fmt.Errorf("dnsproxy: upstream response does not match its query")
	}
	got, want := response.Question[0], query.Question[0]
	if dns.CanonicalName(got.Name) != want.Name || got.Qtype != want.Qtype || got.Qclass != want.Qclass {
		return nil, fmt.Errorf("dnsproxy: upstream response question does not match its query")
	}
	return response, nil
}

// LookupNetIP implements the net.Resolver method used by egress policies.
// It follows at most sixteen CNAMEs, keeps one deadline across both address
// families and all aliases, and accepts addresses only for the queried name
// or a CNAME reached from it. network is "ip", "ip4", or "ip6".
func (r *Resolver) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	types := []uint16{dns.TypeA, dns.TypeAAAA}
	switch network {
	case "ip":
	case "ip4":
		types = types[:1]
	case "ip6":
		types = types[1:]
	default:
		return nil, net.UnknownNetworkError(network)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		addr = addr.Unmap()
		if network == "ip" || network == "ip4" && addr.Is4() || network == "ip6" && addr.Is6() {
			return []netip.Addr{addr}, nil
		}
		return nil, &net.DNSError{Name: host, Err: "no address for requested family", IsNotFound: true}
	}
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	type result struct {
		addrs []netip.Addr
		err   error
	}
	results := make(chan result, len(types))
	for _, typ := range types {
		go func() {
			addrs, err := r.lookup(ctx, host, typ)
			results <- result{addrs, err}
		}()
	}
	var addrs []netip.Addr
	var lastErr error
	for range types {
		result := <-results
		addrs = append(addrs, result.addrs...)
		if result.err != nil {
			lastErr = result.err
		}
	}
	// A broken or unavailable address family must not discard successful
	// answers from the other one. Explicit caller cancellation still wins.
	if len(addrs) != 0 && !errors.Is(ctx.Err(), context.Canceled) {
		slices.SortFunc(addrs, netip.Addr.Compare)
		return slices.Compact(addrs), nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if lastErr == nil {
		lastErr = &net.DNSError{Name: host, Err: "no address records", IsNotFound: true}
	}
	return nil, lastErr
}

func (r *Resolver) lookup(ctx context.Context, host string, typ uint16) ([]netip.Addr, error) {
	name := host
	seen := map[string]bool{dns.CanonicalName(host): true}
	for {
		response, err := r.Query(ctx, name, typ)
		if err != nil {
			return nil, err
		}
		if response.Rcode != dns.RcodeSuccess {
			return nil, &net.DNSError{Name: host, Err: dns.RcodeToString[response.Rcode], IsNotFound: response.Rcode == dns.RcodeNameError, IsTemporary: response.Rcode == dns.RcodeServerFailure}
		}
		name = dns.CanonicalName(name)
		current := name
		for {
			var addrs []netip.Addr
			var target string
			for _, rr := range response.Answer {
				if rr.Header().Class != dns.ClassINET || dns.CanonicalName(rr.Header().Name) != current {
					continue
				}
				switch rr := rr.(type) {
				case *dns.A:
					if addr, ok := netip.AddrFromSlice(rr.A); ok && typ == dns.TypeA {
						addrs = append(addrs, addr.Unmap())
					}
				case *dns.AAAA:
					if addr, ok := netip.AddrFromSlice(rr.AAAA); ok && typ == dns.TypeAAAA {
						addrs = append(addrs, addr)
					}
				case *dns.CNAME:
					alias := dns.CanonicalName(rr.Target)
					if target != "" && target != alias {
						return nil, fmt.Errorf("dnsproxy: conflicting CNAMEs for %q", current)
					}
					target = alias
				}
			}
			if len(addrs) != 0 {
				return addrs, nil
			}
			if target == "" {
				break
			}
			if seen[target] || len(seen) > maxAliases {
				return nil, fmt.Errorf("dnsproxy: CNAME chain for %q loops or exceeds %d aliases", host, maxAliases)
			}
			seen[target] = true
			current = target
		}
		if current == name {
			return nil, nil
		}
		name = current
	}
}

type resolverKey struct{}

// WithResolver carries a network's DNS choice into its host-side egress dial.
func WithResolver(ctx context.Context, resolver *Resolver) context.Context {
	return context.WithValue(ctx, resolverKey{}, resolver)
}

// FromContext returns the network's resolver, or nil when none was provided.
func FromContext(ctx context.Context) *Resolver {
	resolver, _ := ctx.Value(resolverKey{}).(*Resolver)
	return resolver
}
