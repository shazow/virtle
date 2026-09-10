//go:build integration

package e2e

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
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
	defer network.Close()
	g := f.networkGuest(t, network)
	spec := g.spec(t)
	forward := freeLoopbackPort(t)
	spec.Ports = []vm.Forward{{HostAddr: forward, GuestAddr: ":7"}}
	m, log := startReady(t, g, spec)
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

	guestAddr := net.JoinHostPort(status.Networks[0].Addr, "7")
	echoLine(t, func() (net.Conn, error) { return network.DialContext(ctx, "tcp", guestAddr) }, "direct")
	echoLine(t, func() (net.Conn, error) { return net.DialTimeout("tcp", forward, readyTimeout) }, "forwarded")

	attacher := m.(backend.DeviceAttacher)
	extra := vm.Forward{HostAddr: freeLoopbackPort(t), GuestAddr: ":7"}
	if err := attacher.Attach(ctx, extra); err != nil {
		t.Fatalf("Attach forward: %v", err)
	}
	echoLine(t, func() (net.Conn, error) { return net.DialTimeout("tcp", extra.HostAddr, readyTimeout) }, "attached")
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
	if err := m.Kill(); err != nil {
		t.Fatal(err)
	}
	waitExit(t, m)
	// The port went with the machine: its address has nothing behind it, so
	// a dial gets no answer at all rather than a refusal.
	gone, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if c, err := network.DialContext(gone, "tcp", guestAddr); err == nil {
		c.Close()
		t.Fatal("the port outlived the machine")
	}
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
	defer network.Close()
	g := f.networkGuest(t, network)
	spec := g.spec(t)
	spec.Kernel.Cmdline = fmt.Sprintf("%s virtle.egress=%d", fixtureCmdline, port)
	_, log := startReady(t, g, spec)

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
