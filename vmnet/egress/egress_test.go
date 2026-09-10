package egress

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/shazow/virtle/vm"
	"github.com/shazow/virtle/vmnet"
)

// hosts resolves names from a table; unknown names do not exist.
type hosts map[string][]netip.Addr

func (h hosts) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if addrs, ok := h[strings.TrimSuffix(host, ".")]; ok {
		return addrs, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

type events struct {
	mu   sync.Mutex
	list []Event
}

func (e *events) Record(ev Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.list = append(e.list, ev)
}

func (e *events) last(t *testing.T) Event {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.list) == 0 {
		t.Fatal("no event recorded")
	}
	return e.list[len(e.list)-1]
}

// echoServer listens on the loopback and echoes; it returns the port.
func echoServer(t *testing.T) uint16 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	return netip.MustParseAddrPort(ln.Addr().String()).Port()
}

func namedFlow(host string, port uint16) vmnet.Flow {
	return vmnet.Flow{Guest: "vm1", Src: netip.MustParseAddrPort("192.168.127.2:40000"), Dst: netip.AddrPortFrom(netip.MustParseAddr("198.18.0.5"), port), Host: host}
}

func addrFlow(dst netip.AddrPort) vmnet.Flow {
	return vmnet.Flow{Guest: "vm1", Src: netip.MustParseAddrPort("192.168.127.2:40000"), Dst: dst}
}

func expectEcho(t *testing.T, c net.Conn) {
	t.Helper()
	defer c.Close()
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo = %q, %v", buf, err)
	}
}

func TestPolicyDecidesByNameAndPort(t *testing.T) {
	port := echoServer(t)
	rec := &events{}
	loopback := netip.MustParseAddr("127.0.0.1")
	p := &Policy{
		Rules:        []Rule{{Hosts: []string{"*.example.test"}, Ports: []int{int(port)}}},
		DenyPrefixes: []netip.Prefix{},
		Resolver:     hosts{"api.example.test": {loopback}, "example.test": {loopback}},
		Recorder:     rec,
	}
	ctx := context.Background()
	c, err := p.DialFlow(ctx, namedFlow("api.example.test", port))
	if err != nil {
		t.Fatalf("allowed flow: %v", err)
	}
	expectEcho(t, c)
	if ev := rec.last(t); ev.Decision != Allowed || ev.Rule != "*.example.test" || ev.Host != "api.example.test" || ev.Upstream != netip.AddrPortFrom(loopback, port) || ev.Guest != "vm1" || ev.Proto != vm.TCP {
		t.Errorf("event = %+v", ev)
	}
	for name, f := range map[string]vmnet.Flow{
		"apex is not under the pattern": namedFlow("example.test", port),
		"other port":                    namedFlow("api.example.test", port+1),
		"address flow":                  addrFlow(netip.AddrPortFrom(loopback, port)),
		"unrelated name":                namedFlow("evil.test", port),
	} {
		if _, err := p.DialFlow(ctx, f); !errors.Is(err, vmnet.ErrDenied) {
			t.Errorf("%s: %v, want ErrDenied", name, err)
		}
		if ev := rec.last(t); ev.Decision != Denied || ev.Reason == "" {
			t.Errorf("%s: event = %+v", name, ev)
		}
	}
	// Case and trailing dots do not matter.
	c, err = p.DialFlow(ctx, namedFlow("API.Example.Test.", port))
	if err != nil {
		t.Fatalf("case-insensitive match: %v", err)
	}
	c.Close()
}

func TestPolicyAddressRulesAndDenyPrefixes(t *testing.T) {
	port := echoServer(t)
	dst := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), port)
	rec := &events{}
	p := &Policy{Rules: []Rule{{Hosts: []string{"127.0.0.0/8"}}}, Recorder: rec}
	// The default deny ranges win over a rule.
	if _, err := p.DialFlow(context.Background(), addrFlow(dst)); !errors.Is(err, vmnet.ErrDenied) {
		t.Fatalf("loopback with default deny ranges: %v", err)
	}
	if ev := rec.last(t); !strings.Contains(ev.Reason, "denied range") {
		t.Errorf("event = %+v", ev)
	}
	p.DenyPrefixes = []netip.Prefix{}
	c, err := p.DialFlow(context.Background(), addrFlow(dst))
	if err != nil {
		t.Fatalf("loopback with no deny ranges: %v", err)
	}
	expectEcho(t, c)
	if ev := rec.last(t); ev.Rule != "127.0.0.0/8" || ev.Upstream != dst {
		t.Errorf("event = %+v", ev)
	}
	// A single address is a /32; an address rule never matches a named flow.
	p.Rules = []Rule{{Hosts: []string{"127.0.0.1"}}}
	if _, err := p.DialFlow(context.Background(), addrFlow(dst)); err != nil {
		t.Fatalf("single address rule: %v", err)
	}
	if _, err := p.DialFlow(context.Background(), namedFlow("anything.test", port)); !errors.Is(err, vmnet.ErrDenied) {
		t.Fatalf("address rule matched a name: %v", err)
	}
	if _, err := (&Policy{}).DialFlow(context.Background(), addrFlow(netip.MustParseAddrPort("203.0.113.1:80"))); !errors.Is(err, vmnet.ErrDenied) {
		t.Fatalf("the zero Policy allowed a flow: %v", err)
	}
}

func TestPolicyDeniesNamesResolvingIntoDeniedRanges(t *testing.T) {
	port := echoServer(t)
	rec := &events{}
	p := &Policy{
		Rules: []Rule{{Hosts: []string{"*.test"}}},
		Resolver: hosts{
			"meta.test":  {netip.MustParseAddr("169.254.169.254")},
			"mixed.test": {netip.MustParseAddr("169.254.169.254"), netip.MustParseAddr("127.0.0.1")},
		},
		Recorder: rec,
	}
	if _, err := p.DialFlow(context.Background(), namedFlow("meta.test", port)); !errors.Is(err, vmnet.ErrDenied) {
		t.Fatalf("metadata by name: %v, want ErrDenied", err)
	}
	if ev := rec.last(t); ev.Decision != Denied || !strings.Contains(ev.Reason, "169.254.169.254") {
		t.Errorf("event = %+v", ev)
	}
	// Only the denied addresses are skipped; loopback is denied by default
	// too, so nothing remains.
	if _, err := p.DialFlow(context.Background(), namedFlow("mixed.test", port)); !errors.Is(err, vmnet.ErrDenied) {
		t.Fatalf("mixed resolution: %v", err)
	}
	p.DenyPrefixes = []netip.Prefix{netip.MustParsePrefix("169.254.0.0/16")}
	c, err := p.DialFlow(context.Background(), namedFlow("mixed.test", port))
	if err != nil {
		t.Fatalf("mixed resolution with loopback allowed: %v", err)
	}
	expectEcho(t, c)
	if ev := rec.last(t); ev.Upstream.Addr() != netip.MustParseAddr("127.0.0.1") {
		t.Errorf("dialed %s, want the allowed address", ev.Upstream)
	}
	// A name that does not resolve is an allowed flow whose dial failed.
	if _, err := p.DialFlow(context.Background(), namedFlow("nope.test", port)); err == nil || errors.Is(err, vmnet.ErrDenied) {
		t.Fatalf("unknown name: %v", err)
	}
	if ev := rec.last(t); ev.Decision != Allowed || ev.Err == nil {
		t.Errorf("event = %+v", ev)
	}
}

func TestGuestPolicyOnlyNarrows(t *testing.T) {
	port := echoServer(t)
	loopback := netip.MustParseAddr("127.0.0.1")
	p := &Policy{
		Rules:        []Rule{{Hosts: []string{"*.test"}}},
		DenyPrefixes: []netip.Prefix{},
		Resolver:     hosts{"a.test": {loopback}, "b.test": {loopback}, "a.example": {loopback}},
	}
	dial := func(host string, guest *vm.Egress) error {
		f := namedFlow(host, port)
		f.Egress = guest
		c, err := p.DialFlow(context.Background(), f)
		if err == nil {
			c.Close()
		}
		return err
	}
	if err := dial("a.test", nil); err != nil {
		t.Fatalf("no guest policy: %v", err)
	}
	narrow := &vm.Egress{Allow: []vm.Reach{{Host: "a.test", Ports: []int{int(port)}}}}
	if err := dial("a.test", narrow); err != nil {
		t.Fatalf("within the guest's allow list: %v", err)
	}
	if err := dial("b.test", narrow); !errors.Is(err, vmnet.ErrDenied) {
		t.Fatalf("outside the guest's allow list: %v", err)
	}
	// The guest cannot widen: a.example is not in the host's rules.
	if err := dial("a.example", &vm.Egress{Allow: []vm.Reach{{Host: "*"}}}); !errors.Is(err, vmnet.ErrDenied) {
		t.Fatalf("guest widened the policy: %v", err)
	}
	deny := &vm.Egress{Allow: []vm.Reach{{Host: "*.test"}}, Deny: []vm.Reach{{Host: "b.test"}}}
	if err := dial("b.test", deny); !errors.Is(err, vmnet.ErrDenied) {
		t.Fatalf("guest deny did not win: %v", err)
	}
	if err := dial("a.test", deny); err != nil {
		t.Fatalf("guest allow with an unrelated deny: %v", err)
	}
	if err := dial("a.test", &vm.Egress{}); !errors.Is(err, vmnet.ErrDenied) {
		t.Fatalf("an empty guest policy allowed a flow: %v", err)
	}
}

func TestDefaultRecorderLogs(t *testing.T) {
	var logs bytes.Buffer
	p := &Policy{Logger: slog.New(slog.NewTextHandler(&logs, nil))}
	if _, err := p.DialFlow(context.Background(), addrFlow(netip.MustParseAddrPort("203.0.113.1:80"))); !errors.Is(err, vmnet.ErrDenied) {
		t.Fatal(err)
	}
	if got := logs.String(); !strings.Contains(got, "decision=deny") || !strings.Contains(got, "dst=203.0.113.1:80") || !strings.Contains(got, "guest=vm1") {
		t.Fatalf("log = %q", got)
	}
	silent := &Policy{}
	if _, err := silent.DialFlow(context.Background(), addrFlow(netip.MustParseAddrPort("203.0.113.1:80"))); !errors.Is(err, vmnet.ErrDenied) {
		t.Fatal(err)
	}
}

func TestValidPattern(t *testing.T) {
	for _, ok := range []string{"*.github.com", "api.github.com", "*", "10.0.0.0/8", "192.0.2.1", "fd00::/8"} {
		if err := ValidPattern(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "[", "host:443", "http://x", "a b"} {
		if err := ValidPattern(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
