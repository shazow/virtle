package userspace

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/shazow/virtle/vmnet"
	"github.com/shazow/virtle/vmnet/egress"
)

type recordedEvents struct {
	mu   sync.Mutex
	list []egress.Event
}

func (r *recordedEvents) Record(e egress.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.list = append(r.list, e)
}

// TestFakeIPNamesReachThePolicy exercises synthetic DNS end to end: the guest
// resolves names to synthetic addresses, its flows carry the names, and a
// policy decides by name and dials the real destination.
func TestFakeIPNamesReachThePolicy(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go serveEcho(ln)
	port := netip.MustParseAddrPort(ln.Addr().String()).Port()
	loopback := netip.MustParseAddr("127.0.0.1")
	rec := &recordedEvents{}
	policy := &egress.Policy{
		Rules:        []egress.Rule{{Hosts: []string{"allowed.test", "another.test"}}},
		DenyPrefixes: []netip.Prefix{},
		Recorder:     rec,
	}
	n := newTestNetwork(t, Config{Egress: policy, DNSUpstream: dnsUpstream(t, addressDNS)})
	g := attachGuest(t, n, "vm1", vmnet.AttachOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	resolve := func(name string) netip.Addr {
		t.Helper()
		u, err := g.dialUDP(netip.AddrPortFrom(n.gateway, dnsPort))
		if err != nil {
			t.Fatal(err)
		}
		defer u.Close()
		m := new(dns.Msg)
		m.SetQuestion(dns.Fqdn(name), dns.TypeA)
		r, _, err := (&dns.Client{Timeout: testTimeout}).ExchangeWithConnContext(ctx, m, &dns.Conn{Conn: u})
		if err != nil {
			t.Fatalf("resolve %s: %v", name, err)
		}
		if len(r.Answer) != 1 {
			t.Fatalf("resolve %s: %v", name, r)
		}
		a, ok := netip.AddrFromSlice(r.Answer[0].(*dns.A).A)
		if !ok || !DefaultFakeIPRange.Contains(a) {
			t.Fatalf("resolve %s = %s, want a synthetic address", name, a)
		}
		return a
	}
	allowed, another := resolve("allowed.test"), resolve("another.test")
	if allowed == another || resolve("allowed.test") != allowed {
		t.Fatalf("names map to %s and %s; the mapping must be distinct and stable", allowed, another)
	}
	// A guest can learn a synthetic address from another guest; connection
	// admission must still check the name independently of DNS admission.
	blocked, _ := n.fakeIPs.addr("blocked.test")

	c, err := g.dialTCP(ctx, netip.AddrPortFrom(allowed, port))
	if err != nil {
		t.Fatalf("dial the allowed name: %v", err)
	}
	echo(t, c, "by name")
	c.Close()
	start := time.Now()
	if c, err := g.dialTCP(ctx, netip.AddrPortFrom(blocked, port)); err == nil {
		c.Close()
		t.Fatal("the blocked name connected")
	} else if !strings.Contains(err.Error(), "refused") || time.Since(start) > testTimeout/2 {
		t.Fatalf("blocked name failed with %v after %s, want a prompt refusal", err, time.Since(start))
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.list) != 2 {
		t.Fatalf("events = %+v, want one allow and one deny", rec.list)
	}
	if e := rec.list[0]; e.Decision != egress.Allowed || e.Host != "allowed.test" || e.Guest != "vm1" || e.Upstream != netip.AddrPortFrom(loopback, port) {
		t.Errorf("allow event = %+v", e)
	}
	if e := rec.list[1]; e.Decision != egress.Denied || e.Host != "blocked.test" || e.Dst.Addr() != blocked {
		t.Errorf("deny event = %+v", e)
	}

	// The synthetic address resolves back to its name.
	u, err := g.dialUDP(netip.AddrPortFrom(n.gateway, dnsPort))
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	m := new(dns.Msg)
	arpa, _ := dns.ReverseAddr(allowed.String())
	m.SetQuestion(arpa, dns.TypePTR)
	r, _, err := (&dns.Client{Timeout: testTimeout}).ExchangeWithConnContext(ctx, m, &dns.Conn{Conn: u})
	if err != nil || len(r.Answer) != 1 || r.Answer[0].(*dns.PTR).Ptr != "allowed.test." {
		t.Fatalf("PTR = %v, %v", r, err)
	}
}

// hostTable resolves names for the policy; it never sees synthetic addresses.
type hostTable map[string]netip.Addr

func (h hostTable) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if a, ok := h[strings.TrimSuffix(host, ".")]; ok {
		return []netip.Addr{a}, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

func TestFakeIPRange(t *testing.T) {
	for name, cfg := range map[string]Config{
		"fake range overlaps subnet": {FakeIPRange: netip.MustParsePrefix("192.168.0.0/16")},
		"fake range is ipv6":         {FakeIPRange: netip.MustParsePrefix("fd00::/64")},
	} {
		if n, err := New(cfg); err == nil {
			n.Close()
			t.Errorf("New accepted %s", name)
		}
	}
	n := newTestNetwork(t, Config{FakeIPRange: netip.MustParsePrefix("10.99.0.0/16")})
	if !n.fakeIPs.contains(netip.MustParseAddr("10.99.3.4")) {
		t.Fatal("the configured fake range is not in use")
	}

}

func TestUnknownFakeAddressesAreRefused(t *testing.T) {
	egress := &recordingEgress{dial: vmnet.Passthrough{}.DialFlow}
	n := newTestNetwork(t, Config{Egress: egress})
	g := attachGuest(t, n, "vm1", vmnet.AttachOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	// Nothing resolved to this address: it stands for no name.
	if c, err := g.dialTCP(ctx, netip.MustParseAddrPort("198.18.7.7:80")); err == nil {
		c.Close()
		t.Fatal("a flow to a synthetic address never handed out connected")
	} else if !strings.Contains(err.Error(), "refused") {
		t.Fatalf("dial = %v, want a refusal", err)
	}
	egress.mu.Lock()
	defer egress.mu.Unlock()
	if len(egress.flows) != 0 {
		t.Errorf("the egress saw %d flows for an address without a name, want none", len(egress.flows))
	}
}
