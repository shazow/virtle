package userspace

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/shazow/virtle/internal/networkstate"
	"github.com/shazow/virtle/vmnet"
	"github.com/shazow/virtle/vmnet/egress"
)

func roundTripNetworkState(t *testing.T, state networkstate.State) networkstate.State {
	t.Helper()
	var encoded bytes.Buffer
	if err := json.NewEncoder(&encoded).Encode(state); err != nil {
		t.Fatal(err)
	}
	var restored networkstate.State
	if err := json.NewDecoder(&encoded).Decode(&restored); err != nil {
		t.Fatal(err)
	}
	return restored
}

func TestNetworkStatePreservesDNSBindings(t *testing.T) {
	cfg := Config{FakeIPRange: netip.MustParsePrefix("198.18.0.0/29"), DNSUpstream: dnsUpstream(t, addressDNS)}
	before := newTestNetwork(t, cfg)
	first := attachGuest(t, before, "guest", vmnet.AttachOptions{})
	addresses := make(map[string]string)
	for _, name := range []string{"one.test", "two.test"} {
		r := queryDNS(t, first, "udp", name, dns.TypeA)
		if len(r.Answer) != 1 {
			t.Fatal(r)
		}
		addresses[name] = r.Answer[0].(*dns.A).A.String()
	}
	if err := first.port.Close(); err != nil {
		t.Fatal(err)
	}
	wantNext := before.fakeIPs.next
	saved := roundTripNetworkState(t, before.SaveNetworkState())
	if err := before.Close(); err != nil {
		t.Fatal(err)
	}
	after := newTestNetwork(t, cfg)
	if err := after.RestoreNetworkState(saved); err != nil {
		t.Fatal(err)
	}
	if after.fakeIPs.next != wantNext {
		t.Fatalf("allocation cursor = %s, want %s", after.fakeIPs.next, wantNext)
	}
	resumed := attachGuest(t, after, "guest", vmnet.AttachOptions{Addr: first.addr})
	for name, want := range addresses {
		r := queryDNS(t, resumed, "udp", name, dns.TypeA)
		if len(r.Answer) != 1 || r.Answer[0].(*dns.A).A.String() != want {
			t.Fatalf("restored %s = %v, want %s", name, r, want)
		}
		reverse, _ := dns.ReverseAddr(want)
		ptr := queryDNS(t, resumed, "udp", reverse, dns.TypePTR)
		if len(ptr.Answer) != 1 || ptr.Answer[0].(*dns.PTR).Ptr != dns.Fqdn(name) {
			t.Fatalf("restored PTR = %v, want %s", ptr, name)
		}
	}
	third := queryDNS(t, resumed, "udp", "three.test", dns.TypeA)
	if len(third.Answer) != 1 || third.Answer[0].(*dns.A).A.String() != wantNext.String() {
		t.Fatalf("next new address = %v, want %s", third, wantNext)
	}
}

func TestNetworkStateRenewsDNSLifetimeAfterDowntime(t *testing.T) {
	cfg := Config{FakeIPRange: netip.MustParsePrefix("198.18.0.0/30")}
	before := newTestNetwork(t, cfg)
	started := time.Unix(0, 0)
	before.fakeIPs.now = func() time.Time { return started }
	one, _ := before.fakeIPs.addr("one.test")
	_, _ = before.fakeIPs.addr("two.test")
	// Even expired bindings remain meaningful until their address is
	// actually recycled; suspend must preserve those names as well.
	started = started.Add(2 * dnsTTL * time.Second)
	saved := roundTripNetworkState(t, before.SaveNetworkState())
	_ = before.Close()
	after := newTestNetwork(t, cfg)
	var clock atomic.Int64
	clock.Store(started.Add(365 * 24 * time.Hour).UnixNano())
	after.fakeIPs.now = func() time.Time { return time.Unix(0, clock.Load()) }
	if err := after.RestoreNetworkState(saved); err != nil {
		t.Fatal(err)
	}
	if addr, ok := after.fakeIPs.addr("three.test"); ok {
		t.Fatalf("downtime expired a guest's cached binding: %s", addr)
	}
	clock.Add(int64(dnsTTL*time.Second - time.Nanosecond))
	if addr, ok := after.fakeIPs.addr("three.test"); ok {
		t.Fatalf("restored binding expired before a full TTL: %s", addr)
	}
	clock.Add(1)
	if addr, ok := after.fakeIPs.addr("three.test"); !ok || addr != one {
		t.Fatalf("restored allocation after TTL = %s, %v; want %s", addr, ok, one)
	}
}

func TestNetworkStateRejectsInvalidBindingsAtomically(t *testing.T) {
	prefix := netip.MustParsePrefix("198.18.0.0/29")
	binding := func(name, addr string) networkstate.Binding {
		return networkstate.Binding{Name: name, Addr: netip.MustParseAddr(addr)}
	}
	for name, invalid := range map[string]networkstate.State{
		"different range": {FakeIPRange: netip.MustParsePrefix("198.18.1.0/29")},
		"empty name":      {Bindings: []networkstate.Binding{binding("", "198.18.0.3")}},
		"noncanonical":    {Bindings: []networkstate.Binding{binding("Other.Test.", "198.18.0.3")}},
		"empty label":     {Bindings: []networkstate.Binding{binding("other..test", "198.18.0.3")}},
		"long label":      {Bindings: []networkstate.Binding{binding(strings.Repeat("a", 64)+".test", "198.18.0.3")}},
		"invalid address": {Bindings: []networkstate.Binding{{Name: "other.test"}}},
		"outside range":   {Bindings: []networkstate.Binding{binding("other.test", "198.18.1.1")}},
		"network address": {Bindings: []networkstate.Binding{binding("other.test", "198.18.0.0")}},
		"broadcast":       {Bindings: []networkstate.Binding{binding("other.test", "198.18.0.7")}},
		"duplicate name":  {Bindings: []networkstate.Binding{binding("new.test", "198.18.0.3")}},
		"duplicate addr":  {Bindings: []networkstate.Binding{binding("other.test", "198.18.0.2")}},
		"name conflict":   {Bindings: []networkstate.Binding{binding("existing.test", "198.18.0.3")}},
		"addr conflict":   {Bindings: []networkstate.Binding{binding("other.test", "198.18.0.1")}},
		"unknown token":   {Tokens: map[string]string{"UNKNOWN": "saved-placeholder"}},
		"empty token":     {Tokens: map[string]string{"TOKEN": ""}},
	} {
		t.Run(name, func(t *testing.T) {
			policy := checkpointPolicy(t, "current-secret")
			n := newTestNetwork(t, Config{FakeIPRange: prefix, Egress: policy})
			_, _ = n.fakeIPs.addr("existing.test")
			_ = policy.GuestEnv(nil)
			before := n.SaveNetworkState()
			next := n.fakeIPs.next
			expires := n.fakeIPs.used.Front().Value.(*fakeIPEntry).expires
			if !invalid.FakeIPRange.IsValid() {
				invalid.FakeIPRange = prefix
			}
			// Validate a good new binding first, then encounter the error.
			invalid.Bindings = append([]networkstate.Binding{binding("new.test", "198.18.0.2")}, invalid.Bindings...)
			if err := n.RestoreNetworkState(invalid); err == nil {
				t.Fatal("invalid checkpoint was accepted")
			}
			if after := n.SaveNetworkState(); !reflect.DeepEqual(after, before) || n.fakeIPs.next != next || n.fakeIPs.used.Front().Value.(*fakeIPEntry).expires != expires {
				t.Fatalf("rejected restore changed state: before=%+v, after=%+v", before, after)
			}
		})
	}
}

func TestNetworkStateMergesCompatibleGuests(t *testing.T) {
	policy := checkpointPolicy(t, "current-secret")
	n := newTestNetwork(t, Config{Egress: policy})
	addr, _ := n.fakeIPs.addr("existing.test")
	_ = policy.GuestEnv(nil)
	_ = attachGuest(t, n, "running", vmnet.AttachOptions{})
	state := n.SaveNetworkState()
	state.Bindings = append(state.Bindings, networkstate.Binding{Name: "restored.test", Addr: addr.Next()})
	if err := n.RestoreNetworkState(state); err != nil {
		t.Fatalf("compatible checkpoint could not join an active network: %v", err)
	}
	for _, b := range state.Bindings {
		if name, ok := n.fakeIPs.name(b.Addr); !ok || name != b.Name {
			t.Fatalf("merged binding %s = %q, %v", b.Addr, name, ok)
		}
	}
	before := n.SaveNetworkState()
	state.Tokens["TOKEN"] = "different-placeholder"
	state.Bindings = append(state.Bindings, networkstate.Binding{Name: "new.test", Addr: addr.Next().Next()})
	if err := n.RestoreNetworkState(state); err == nil {
		t.Fatal("active network's issued token was replaced")
	}
	if after := n.SaveNetworkState(); !reflect.DeepEqual(after, before) {
		t.Fatalf("token conflict changed active network state: before=%+v, after=%+v", before, after)
	}
}

func TestNetworkStateRequiresTokenRestoreSupport(t *testing.T) {
	n := newTestNetwork(t, Config{Egress: vmnet.DenyAll{}})
	before := n.SaveNetworkState()
	saved := networkstate.State{FakeIPRange: before.FakeIPRange,
		Bindings: []networkstate.Binding{{Name: "api.test", Addr: before.FakeIPRange.Addr().Next()}},
		Tokens:   map[string]string{"TOKEN": "placeholder"},
	}
	if err := n.RestoreNetworkState(saved); err == nil {
		t.Fatal("saved tokens were silently discarded by the current egress")
	}
	if after := n.SaveNetworkState(); !reflect.DeepEqual(after, before) {
		t.Fatalf("unsupported token restore changed DNS bindings: %+v", after)
	}
}

func checkpointPolicy(t *testing.T, value string) *egress.Policy {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{
		SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &egress.Policy{
		Rules:        []egress.Rule{{Hosts: []string{"api.test"}, Inspect: true}},
		DenyPrefixes: []netip.Prefix{},
		CA:           tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key},
		Injections: []egress.Injection{{Name: "TOKEN", Hosts: []string{"api.test"}, In: []egress.Placement{egress.InHeader},
			Value: func(context.Context, egress.Request) (string, error) { return value, nil },
		}},
	}
}

func TestNetworkStatePreservesIssuedSecretTokens(t *testing.T) {
	seen := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("Authorization")
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()
	upstreamAddr := netip.MustParseAddrPort(upstream.Listener.Addr().String())
	dnsAddr := dnsUpstream(t, addressDNS)
	oldPolicy := checkpointPolicy(t, "old-secret")
	before := newTestNetwork(t, Config{Egress: oldPolicy, DNSUpstream: dnsAddr})
	issued := oldPolicy.GuestEnv(nil)
	if len(issued) != 1 {
		t.Fatalf("issued environment = %v", issued)
	}
	token := strings.TrimPrefix(issued[0], "TOKEN=")
	addr, _ := before.fakeIPs.addr("api.test")
	saved := roundTripNetworkState(t, before.SaveNetworkState())
	encoded, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("old-secret")) {
		t.Fatal("checkpoint contains the secret value")
	}
	_ = before.Close()
	currentPolicy := checkpointPolicy(t, "current-secret")
	after := newTestNetwork(t, Config{Egress: currentPolicy, DNSUpstream: dnsAddr})
	if err := after.RestoreNetworkState(saved); err != nil {
		t.Fatal(err)
	}
	if current := currentPolicy.GuestEnv(nil); !reflect.DeepEqual(current, issued) {
		t.Fatalf("restored environment = %v, want %v", current, issued)
	}
	g := attachGuest(t, after, "resumed", vmnet.AttachOptions{})
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()
	c, err := g.dialTCP(ctx, netip.AddrPortFrom(addr, upstreamAddr.Port()))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(testTimeout))
	if _, err := fmt.Fprintf(c, "GET / HTTP/1.1\r\nHost: api.test:%d\r\nAuthorization: Bearer %s\r\nConnection: close\r\n\r\n", upstreamAddr.Port(), token); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-seen:
		if got != "Bearer current-secret" {
			t.Fatalf("restored token substitution = %q", got)
		}
	case <-ctx.Done():
		t.Fatal("restored token request did not reach the upstream")
	}
}
