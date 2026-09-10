//go:build integration

package e2e

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/backend/qemu"
	"github.com/shazow/virtle/vm"
	"github.com/shazow/virtle/vmnet"
	"github.com/shazow/virtle/vmnet/egress"
	"github.com/shazow/virtle/vmnet/userspace"
)

// The network scenarios run on QEMU only: a virtle network needs frames from
// the guest NIC, which Firecracker hands to a host TAP device instead (the
// guest daemon will carry them over vsock).

// networkGuest is the QEMU guest with its NIC on network.
func (f fixture) networkGuest(t *testing.T, network vmnet.Network) guest {
	t.Helper()
	for _, g := range f.guests() {
		if g.name != "qemu" {
			continue
		}
		g.newBackend = func(inner func(io.Writer) backend.Backend) func(io.Writer) backend.Backend {
			return func(console io.Writer) backend.Backend {
				b := inner(console).(*qemu.Backend)
				b.Network = network
				return b
			}
		}(g.newBackend)
		return g
	}
	t.Fatal("no qemu guest in the fixture")
	return guest{}
}

var netLine = regexp.MustCompile(`VIRTLE_NET:(\S+)`)

// echoLine dials addr and expects a line to come back as sent.
func echoLine(t *testing.T, dial func() (net.Conn, error), msg string) {
	t.Helper()
	c, err := dial()
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(readyTimeout))
	if _, err := io.WriteString(c, msg+"\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := bufio.NewReader(c).ReadString('\n')
	if err != nil || strings.TrimSpace(got) != msg {
		t.Fatalf("echo = %q, %v; want %q", got, err, msg)
	}
}

func freeLoopbackPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// TestNetwork boots the fixture with its NIC on a virtle network: the guest
// takes the lease the port fixed, the host dials it with and without a
// forward, and forwards attach and detach at runtime.
func TestNetwork(t *testing.T) {
	f := loadFixture(t)
	network, err := userspace.New(userspace.Config{})
	if err != nil {
		t.Fatal(err)
	}
	step := stepTimer(t)
	defer func() { _ = network.Close(); step("network close") }()
	g := f.networkGuest(t, network)
	spec := g.spec(t)
	forward := freeLoopbackPort(t)
	spec.Ports = []vm.Forward{{HostAddr: forward, GuestAddr: ":7"}}
	m, log := startReady(t, g, spec)
	step("ready")
	ctx := context.Background()

	leased := netLine.FindStringSubmatch(log.String())
	if leased == nil || leased[1] == "none" {
		t.Fatalf("the guest reported no lease\n--- console ---\n%s", log.String())
	}
	status, err := m.(backend.StatusReporter).Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Networks) != 1 || !status.Networks[0].Attached || status.Networks[0].Addr != leased[1] {
		t.Fatalf("Status.Networks = %+v, want one attached NIC at the leased %s", status.Networks, leased[1])
	}
	if _, err := net.ParseMAC(status.Networks[0].MAC); err != nil || !network.Subnet().Contains(netip.MustParseAddr(leased[1])) {
		t.Fatalf("NIC %+v is not on %s", status.Networks[0], network.Subnet())
	}

	step("status")
	guestAddr := net.JoinHostPort(status.Networks[0].Addr, "7")
	echoLine(t, func() (net.Conn, error) { return network.DialContext(ctx, "tcp", guestAddr) }, "direct")
	step("direct echo")
	echoLine(t, func() (net.Conn, error) { return net.DialTimeout("tcp", forward, readyTimeout) }, "forwarded")
	step("forwarded echo")

	attacher := m.(backend.DeviceAttacher)
	extra := vm.Forward{HostAddr: freeLoopbackPort(t), GuestAddr: ":7"}
	if err := attacher.Attach(ctx, extra); err != nil {
		t.Fatalf("Attach forward: %v", err)
	}
	echoLine(t, func() (net.Conn, error) { return net.DialTimeout("tcp", extra.HostAddr, readyTimeout) }, "attached")
	step("attached echo")
	if err := attacher.Detach(ctx, extra); err != nil {
		t.Fatalf("Detach forward: %v", err)
	}
	if c, err := net.DialTimeout("tcp", extra.HostAddr, time.Second); err == nil {
		c.Close()
		t.Fatal("the detached forward still accepts")
	}
	if err := attacher.Detach(ctx, spec.Ports[0]); err != nil {
		t.Fatalf("Detach a Spec.Ports forward: %v", err)
	}
	if c, err := net.DialTimeout("tcp", forward, time.Second); err == nil {
		c.Close()
		t.Fatal("the detached Spec forward still accepts")
	}
	step("detached")
	if err := m.Kill(); err != nil {
		t.Fatal(err)
	}
	waitExit(t, m)
	step("exit")
	// The port went with the machine: its address has nothing behind it, so
	// a dial gets no answer at all rather than a refusal.
	gone, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if c, err := network.DialContext(gone, "tcp", guestAddr); err == nil {
		c.Close()
		t.Fatal("the port outlived the machine")
	}
	step("dial after exit")
}

// lineEcho answers one line per connection and hangs up, so a client that
// waits for the server to close (busybox nc) finishes.
func lineEcho(t *testing.T) (port int) {
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
				_ = c.SetDeadline(time.Now().Add(readyTimeout))
				line, err := bufio.NewReader(c).ReadString('\n')
				if err == nil {
					_, _ = io.WriteString(c, line)
				}
			}()
		}
	}()
	return int(netip.MustParseAddrPort(ln.Addr().String()).Port())
}

type hostTable map[string]netip.Addr

func (h hostTable) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if a, ok := h[strings.TrimSuffix(host, ".")]; ok {
		return []netip.Addr{a}, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

type eventLog struct {
	mu   sync.Mutex
	list []egress.Event
}

func (l *eventLog) Record(e egress.Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.list = append(l.list, e)
}

// TestEgressPolicy puts a policy on the guest's network: the guest resolves
// two names through the network's fake-IP DNS and connects to both; the
// allowed one is answered by a host listener and the other is refused
// before it opens, with both decisions on record.
func TestEgressPolicy(t *testing.T) {
	f := loadFixture(t)
	port := lineEcho(t)
	events := &eventLog{}
	loopback := netip.MustParseAddr("127.0.0.1")
	policy := &egress.Policy{
		Rules:        []egress.Rule{{Hosts: []string{"allowed.test"}, Ports: []int{port}}},
		DenyPrefixes: []netip.Prefix{}, // the listener is on the loopback
		Resolver:     hostTable{"allowed.test": loopback, "blocked.test": loopback},
		Recorder:     events,
	}
	network, err := userspace.New(userspace.Config{DNS: userspace.DNSFakeIP, Egress: policy})
	if err != nil {
		t.Fatal(err)
	}
	step := stepTimer(t)
	defer func() { _ = network.Close(); step("network close") }()
	g := f.networkGuest(t, network)
	spec := g.spec(t)
	spec.Kernel.Cmdline = fmt.Sprintf("%s virtle.egress=%d", fixtureCmdline, port)
	_, log := startReady(t, g, spec)
	step("ready")

	if want := "VIRTLE_EGRESS:allowed=ping,blocked=refused"; !strings.Contains(log.String(), want) {
		t.Fatalf("guest did not report %q\n--- console ---\n%s", want, log.String())
	}
	events.mu.Lock()
	defer events.mu.Unlock()
	var allowed, denied *egress.Event
	for i := range events.list {
		switch e := &events.list[i]; {
		case e.Host == "allowed.test" && e.Decision == egress.Allowed:
			allowed = e
		case e.Host == "blocked.test" && e.Decision == egress.Denied:
			denied = e
		}
	}
	if allowed == nil || denied == nil {
		t.Fatalf("events = %+v, want an allow for allowed.test and a deny for blocked.test", events.list)
	}
	if allowed.Upstream.Port() != uint16(port) || allowed.Err != nil {
		t.Fatalf("allowed event = %+v", *allowed)
	}
	if !network.Subnet().Contains(denied.Src.Addr()) || !userspace.DefaultFakeIPRange.Contains(denied.Dst.Addr()) {
		t.Fatalf("denied event = %+v; the guest dialed a synthetic address from its own", *denied)
	}
}

var (
	injectLine = regexp.MustCompile(`VIRTLE_INJECT:header=([^;\s]*);query=(\S*)`)
	rejectLine = regexp.MustCompile(`VIRTLE_REJECT:(.*)`)
	admitLine  = regexp.MustCompile(`VIRTLE_ADMIT:(.*)`)
)

// randomHex is the kind of Injection.Value a program using virtle brings:
// the library ships no values of its own.
func randomHex(n int) func(context.Context, egress.Request) (string, error) {
	return func(context.Context, egress.Request) (string, error) {
		b := make([]byte, n)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		return hex.EncodeToString(b), nil
	}
}

// TestEgressInjection puts the guest behind an inspecting policy a program
// customized through the public API: $VIRTLE_RANDOM$ is replaced by a
// fresh value per request, $VIRTLE_REJECT$ refuses the request that
// carries it, and Admit refuses a path outright. The server behind the
// policy sees one random value in the header and the query, the guest sees
// the answer and the two refusals, and all three requests are on record.
func TestEgressInjection(t *testing.T) {
	f := loadFixture(t)
	var mu sync.Mutex
	var sawHeader, sawQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sawHeader, sawQuery = r.Header.Get("X-Random"), r.URL.Query().Get("r")
		mu.Unlock()
		fmt.Fprintf(w, "header=%s;query=%s", r.Header.Get("X-Random"), r.URL.Query().Get("r"))
	}))
	defer upstream.Close()
	port := int(netip.MustParseAddrPort(upstream.Listener.Addr().String()).Port())
	ca, err := egress.LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	events := &eventLog{}
	policy := &egress.Policy{
		Rules:        []egress.Rule{{Hosts: []string{"inject.test"}, Ports: []int{port}, Inspect: true}},
		DenyPrefixes: []netip.Prefix{}, // the server is on the loopback
		Resolver:     hostTable{"inject.test": netip.MustParseAddr("127.0.0.1")},
		Recorder:     events,
		CA:           ca,
		Injections: []egress.Injection{
			{Token: "$VIRTLE_RANDOM$", Value: randomHex(8)},
			{Token: "$VIRTLE_REJECT$", Value: func(context.Context, egress.Request) (string, error) { return "", vmnet.ErrDenied }},
		},
		Admit: func(_ context.Context, r egress.Request) error {
			if r.URL.Path == "/forbidden" {
				return fmt.Errorf("%s is off limits: %w", r.URL.Path, vmnet.ErrDenied)
			}
			return nil
		},
	}
	if err := policy.Validate(); err != nil {
		t.Fatal(err)
	}
	network, err := userspace.New(userspace.Config{DNS: userspace.DNSFakeIP, Egress: policy})
	if err != nil {
		t.Fatal(err)
	}
	step := stepTimer(t)
	defer func() { _ = network.Close(); step("network close") }()
	g := f.networkGuest(t, network)
	spec := g.spec(t)
	spec.Kernel.Cmdline = fmt.Sprintf("%s virtle.inject=%d", fixtureCmdline, port)
	_, log := startReady(t, g, spec)
	step("ready")

	m := injectLine.FindStringSubmatch(log.String())
	if m == nil {
		events.mu.Lock()
		defer events.mu.Unlock()
		t.Fatalf("guest did not report the fetch\n--- events ---\n%+v\n--- console ---\n%s", events.list, log.String())
	}
	value := m[1]
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(value) || m[2] != value {
		t.Fatalf("guest saw header=%q query=%q; want one random value in both", m[1], m[2])
	}
	mu.Lock()
	if sawHeader != value || sawQuery != value {
		t.Errorf("server saw header=%q query=%q, want %q", sawHeader, sawQuery, value)
	}
	mu.Unlock()
	for _, probe := range []struct {
		name string
		line *regexp.Regexp
	}{{"reject", rejectLine}, {"admit", admitLine}} {
		m := probe.line.FindStringSubmatch(log.String())
		if m == nil || !strings.Contains(m[1], "403") {
			t.Errorf("%s probe: guest reported %q, want a 403 refusal", probe.name, m)
		}
	}

	events.mu.Lock()
	defer events.mu.Unlock()
	requests := map[string]egress.Event{}
	for _, e := range events.list {
		if e.Host == "inject.test" && e.Method != "" {
			requests[e.Path+"/"+string(e.Decision)] = e
		}
	}
	random, ok := requests["/echo/allow"]
	if !ok || random.Status != 200 || len(random.Injections) != 1 || random.Injections[0] != "$VIRTLE_RANDOM$" {
		t.Errorf("random request event = %+v, want 200 with the token on record", random)
	}
	rejected, ok := requests["/echo/deny"]
	if !ok || rejected.Status != 403 || !strings.Contains(rejected.Reason, "$VIRTLE_REJECT$") {
		t.Errorf("rejected request event = %+v, want 403 refused by the token", rejected)
	}
	forbidden, ok := requests["/forbidden/deny"]
	if !ok || forbidden.Status != 403 || !strings.Contains(forbidden.Reason, "off limits") {
		t.Errorf("admit event = %+v, want 403 with Admit's reason", forbidden)
	}
	if t.Failed() {
		t.Logf("events: %+v", events.list)
	}
}
