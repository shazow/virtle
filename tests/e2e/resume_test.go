//go:build integration

package e2e

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/backend/qemu"
	"github.com/shazow/virtle/manifest"
	"github.com/shazow/virtle/vm"
	"github.com/shazow/virtle/vmnet/egress"
	"github.com/shazow/virtle/vmnet/userspace"
)

// TestResumeNetworkState uses two independent manifest loads and QEMU
// processes. The guest keeps a synthetic address and an issued secret token
// in memory; the resumed request must retain that name and use the new
// backend's secret value, despite its initially different generated token.
func TestResumeNetworkState(t *testing.T) {
	f := loadFixture(t)
	// Manifest policies deny loopback even when a hostname is allowed.
	// Listen on a host interface so the real policy can reach the fixture.
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	var hostAddr netip.Addr
	for _, addr := range addrs {
		prefix, err := netip.ParsePrefix(addr.String())
		if err == nil && prefix.Addr().Is4() && prefix.Addr().IsGlobalUnicast() && !prefix.Addr().IsLoopback() {
			hostAddr = prefix.Addr()
			break
		}
	}
	if !hostAddr.IsValid() {
		// Nix build sandboxes have only loopback. Permit that test server
		// for this serial test while preserving the real manifest loader.
		hostAddr = netip.MustParseAddr("127.0.0.1")
		previous := egress.DefaultDenyPrefixes
		egress.DefaultDenyPrefixes = slices.DeleteFunc(slices.Clone(previous), func(p netip.Prefix) bool {
			return p == netip.MustParsePrefix("127.0.0.0/8")
		})
		t.Cleanup(func() { egress.DefaultDenyPrefixes = previous })
	}
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, r.Header.Get("X-Token"))
	}))
	_ = upstream.Listener.Close()
	upstream.Listener, err = net.Listen("tcp", net.JoinHostPort(hostAddr.String(), "0"))
	if err != nil {
		t.Fatal(err)
	}
	upstream.Start()
	defer upstream.Close()
	port := netip.MustParseAddrPort(upstream.Listener.Addr().String()).Port()
	dnsUpstream := fixtureDNS(t, map[string]netip.Addr{"resume.test": hostAddr})
	dir := t.TempDir()
	logs := new(consoleLog)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("backend diagnostics:\n%s", logs.String())
		}
	})
	g := f.guests()[0] // QEMU owns the suspend migration format.
	text := strings.Replace(f.manifest(g, dir), "networks = []\n", fmt.Sprintf("[[networks]]\ntype = \"virtle\"\ndns = { upstream = %q }\n", dnsUpstream), 1)
	const policy = `
[egress]
reach = "all"
[[egress.allow]]
host = "resume.test"
ports = [%d]
inspect = true
[[egress.secrets]]
name = "RESUME_TOKEN"
from = "{{.Env.VIRTLE_RESUME_TEST_SECRET}}"
hosts = ["resume.test"]
`
	load := func(secret string) (*vm.Spec, *qemu.Backend, string) {
		t.Helper()
		t.Setenv("VIRTLE_RESUME_TEST_SECRET", secret)
		spec, loaded, err := manifest.Load(strings.NewReader(text + fmt.Sprintf(policy, port)))
		if err != nil {
			t.Fatalf("load manifest: %v", err)
		}
		b := loaded.(*qemu.Backend)
		b.Logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
		b.ConsoleOutput = logs
		t.Cleanup(func() { _ = b.Close() })
		var token string
		for _, file := range spec.Files {
			if file.GuestPath != manifest.GuestSecretsPath {
				continue
			}
			data, err := io.ReadAll(file.Content)
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range strings.Split(string(data), "\n") {
				if value, ok := strings.CutPrefix(line, "export RESUME_TOKEN="); ok {
					token = value
				}
			}
		}
		if token == "" || token == secret {
			t.Fatal("manifest did not generate a guest placeholder")
		}
		// The tiny fixture has no guest agent. Supply its placeholder at
		// boot instead of provisioning files; HTTP needs no guest CA.
		spec.Files = nil
		spec.Kernel.Cmdline += fmt.Sprintf(" virtle.resume=%d virtle.token=%s", port, token)
		b.RemoteControl = nil
		return spec, b, token
	}
	request := func(n *userspace.Network, addr, command string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), readyTimeout)
		defer cancel()
		c, err := n.DialContext(ctx, "tcp", addr+":8")
		if err != nil {
			t.Fatalf("guest control dial: %v", err)
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(readyTimeout))
		if _, err := fmt.Fprintln(c, command); err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(c)
		if err != nil {
			t.Fatalf("guest control response: %v", err)
		}
		return string(data)
	}
	checkResponse := func(raw, want string) {
		t.Helper()
		if raw != want {
			t.Fatalf("guest HTTP response = %q, want %q", raw, want)
		}
	}

	step := stepTimer(t)
	spec, first, firstToken := load("before-suspend")
	ready := &readyBackend{guest: g, backend: first}
	machine, err := ready.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = machine.Kill() })
	step("first backend ready")
	status, err := machine.(backend.StatusReporter).Status(context.Background())
	if err != nil || len(status.Networks) != 1 {
		t.Fatalf("network status = %+v, %v", status, err)
	}
	addr := status.Networks[0].Addr
	firstNetwork := first.Network.(*userspace.Network)
	cached := strings.TrimSpace(request(firstNetwork, addr, "probe"))
	if a, err := netip.ParseAddr(cached); err != nil || !userspace.DefaultFakeIPRange.Contains(a) {
		t.Fatalf("guest cached address = %q, want a synthetic address", cached)
	}
	checkResponse(request(firstNetwork, addr, "fetch"), "before-suspend")
	step("cached DNS and generated token verified")
	ctx, cancel := context.WithTimeout(context.Background(), readyTimeout)
	defer cancel()
	if err := machine.(backend.Suspender).Suspend(ctx); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	waitExit(t, machine)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	step("suspended and first network closed")

	spec, second, secondToken := load("after-resume")
	if firstToken == secondToken || first.Network == second.Network {
		t.Fatal("fresh manifest load reused the old network or issued token")
	}
	resumed, err := second.Resume(context.Background(), spec)
	if err != nil {
		t.Fatalf("resume with new backend: %v", err)
	}
	t.Cleanup(func() { _ = resumed.Kill() })
	step("resumed through fresh backend")
	secondNetwork := second.Network.(*userspace.Network)
	if got := strings.TrimSpace(request(secondNetwork, addr, "probe")); got != cached {
		t.Fatalf("guest cached address after resume = %q, want %q", got, cached)
	}
	checkResponse(request(secondNetwork, addr, "fetch"), "after-resume")
	step("cached IP and old token work with fresh host policy")
	if err := resumed.Kill(); err != nil {
		t.Fatal(err)
	}
	waitExit(t, resumed)
}
