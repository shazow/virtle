package userspace

import (
	"context"
	"errors"
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

// TestFakeIPNamesReachThePolicy is the fake-IP mode end to end: the guest
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
		Rules:        []egress.Rule{{Hosts: []string{"allowed.test"}}},
		DenyPrefixes: []netip.Prefix{},
		Resolver:     hostTable{"allowed.test": loopback, "blocked.test": loopback},
		Recorder:     rec,
	}
	n := newTestNetwork(t, Config{DNS: DNSFakeIP, Egress: policy})
	g := attachGuest(t, n, "vm1", vmnet.AttachOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	resolve := func(name string) netip.Addr {
		t.Helper()
		u, err := g.dialUDP(netip.AddrPortFrom(n.Gateway(), dnsPort))
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
	allowed, blocked := resolve("allowed.test"), resolve("blocked.test")
	if allowed == blocked || resolve("allowed.test") != allowed {
		t.Fatalf("names map to %s and %s; the mapping must be distinct and stable", allowed, blocked)
	}

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
	u, err := g.dialUDP(netip.AddrPortFrom(n.Gateway(), dnsPort))
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

func TestFakeIPTable(t *testing.T) {
	table := newFakeIPTable(netip.MustParsePrefix("198.18.0.0/30"))
	a, ok := table.addr("One.Test.")
	if !ok || a != netip.MustParseAddr("198.18.0.1") {
		t.Fatalf("first address = %s, %v", a, ok)
	}
	if again, _ := table.addr("one.test"); again != a {
		t.Fatalf("the same name got %s and %s", a, again)
	}
	if name, ok := table.name(a); !ok || name != "one.test" {
		t.Fatalf("name = %q, %v", name, ok)
	}
	if b, ok := table.addr("two.test"); !ok || b != netip.MustParseAddr("198.18.0.2") {
		t.Fatalf("second address = %s, %v", b, ok)
	}
	if _, ok := table.addr("three.test"); ok {
		t.Fatal("a /30 handed out its broadcast address")
	}
	if _, ok := table.name(netip.MustParseAddr("198.18.0.3")); ok {
		t.Fatal("an unassigned address has a name")
	}

	for name, cfg := range map[string]Config{
		"fake range overlaps subnet": {DNS: DNSFakeIP, FakeIPRange: netip.MustParsePrefix("192.168.0.0/16")},
		"fake range is ipv6":         {DNS: DNSFakeIP, FakeIPRange: netip.MustParsePrefix("fd00::/64")},
	} {
		if n, err := New(cfg); err == nil {
			n.Close()
			t.Errorf("New accepted %s", name)
		}
	}
	n := newTestNetwork(t, Config{DNS: DNSFakeIP, FakeIPRange: netip.MustParsePrefix("10.99.0.0/16")})
	if !n.fakeIPs.contains(netip.MustParseAddr("10.99.3.4")) {
		t.Fatal("the configured fake range is not in use")
	}
	if _, err := New(Config{DNS: "magic"}); !errors.Is(err, err) || err == nil {
		t.Fatal("unknown DNS mode accepted")
	}
}
