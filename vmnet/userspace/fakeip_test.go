package userspace

import (
	"net/netip"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/shazow/virtle/vmnet"
)

func TestFakeIPBindingsRetainDNSLifetime(t *testing.T) {
	table := newFakeIPTable(netip.MustParsePrefix("198.18.0.0/30"))
	now := time.Unix(0, 0)
	table.now = func() time.Time { return now }
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
	b, ok := table.addr("two.test")
	if !ok || b != netip.MustParseAddr("198.18.0.2") {
		t.Fatalf("second address = %s, %v", b, ok)
	}
	if addr, ok := table.addr("three.test"); ok {
		t.Fatalf("full range reassigned an unexpired address: %s", addr)
	}
	now = now.Add(dnsTTL*time.Second - time.Nanosecond)
	if addr, ok := table.addr("three.test"); ok {
		t.Fatalf("binding expired before its advertised TTL: %s", addr)
	}
	now = now.Add(time.Nanosecond)
	if c, ok := table.addr("three.test"); !ok || c != a {
		t.Fatalf("expired address reuse = %s, %v; want %s", c, ok, a)
	}
	if name, ok := table.name(a); !ok || name != "three.test" {
		t.Fatalf("reused address names %q, %v", name, ok)
	}
	if d, ok := table.addr("one.test"); !ok || d != b {
		t.Fatalf("returning name = %s, %v; want other expired address %s", d, ok, b)
	}
	if _, ok := table.name(netip.MustParseAddr("198.18.0.3")); ok {
		t.Fatal("the broadcast address has a name")
	}
}

func TestFakeIPUsesRefreshDNSLifetime(t *testing.T) {
	for _, lookup := range []string{"name", "address"} {
		t.Run(lookup, func(t *testing.T) {
			table := newFakeIPTable(netip.MustParsePrefix("198.18.0.0/30"))
			now := time.Unix(0, 0)
			table.now = func() time.Time { return now }
			a, _ := table.addr("one.test")
			b, _ := table.addr("two.test")
			now = now.Add(dnsTTL * time.Second / 2)
			if lookup == "name" {
				if current, ok := table.addr("one.test"); !ok || current != a {
					t.Fatal("repeated name lookup changed its address")
				}
			} else if name, ok := table.name(a); !ok || name != "one.test" {
				t.Fatal("reverse lookup changed its name")
			}
			now = now.Add(dnsTTL * time.Second / 2)
			if c, ok := table.addr("three.test"); !ok || c != b {
				t.Fatalf("reuse = %s, %v; want untouched address %s", c, ok, b)
			}
			now = now.Add(dnsTTL*time.Second/2 - time.Nanosecond)
			if addr, ok := table.addr("four.test"); ok {
				t.Fatalf("refreshed binding was reused before its new TTL: %s", addr)
			}
			now = now.Add(time.Nanosecond)
			if addr, ok := table.addr("four.test"); !ok || addr != a {
				t.Fatalf("reuse after refreshed TTL = %s, %v; want %s", addr, ok, a)
			}
		})
	}
}

func TestDNSFullSyntheticRangePreservesAnswers(t *testing.T) {
	n := newTestNetwork(t, Config{DNS: DNSFakeIP, FakeIPRange: netip.MustParsePrefix("198.18.0.0/30"), DNSUpstream: dnsUpstream(t, addressDNS)})
	g := attachGuest(t, n, "guest", vmnet.AttachOptions{})
	one := queryDNS(t, g, "udp", "one.test", dns.TypeA)
	two := queryDNS(t, g, "udp", "two.test", dns.TypeA)
	if len(one.Answer) != 1 || len(two.Answer) != 1 {
		t.Fatalf("initial answers = %v, %v", one, two)
	}
	if three := queryDNS(t, g, "udp", "three.test", dns.TypeA); three.Rcode != dns.RcodeServerFailure || len(three.Answer) != 0 {
		t.Fatalf("full range answer = %v", three)
	}
	if again := queryDNS(t, g, "udp", "one.test", dns.TypeA); len(again.Answer) != 1 || again.Answer[0].String() != one.Answer[0].String() {
		t.Fatalf("existing answer changed under exhaustion: %v", again)
	}
	for _, r := range []*dns.Msg{one, two} {
		a := r.Answer[0].(*dns.A)
		if a.Hdr.Ttl != dnsTTL {
			t.Fatalf("advertised TTL = %d, want protected lifetime %d", a.Hdr.Ttl, dnsTTL)
		}
		reverse, _ := dns.ReverseAddr(a.A.String())
		ptr := queryDNS(t, g, "udp", reverse, dns.TypePTR)
		if len(ptr.Answer) != 1 || ptr.Answer[0].(*dns.PTR).Ptr != r.Question[0].Name {
			t.Fatalf("cached address changed its name: %v", ptr)
		}
	}
}
