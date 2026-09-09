//go:build integration

package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/backend/backendtest"
	"github.com/shazow/virtle/backend/firecracker"
	"github.com/shazow/virtle/backend/qemu"
	"github.com/shazow/virtle/units"
	"github.com/shazow/virtle/vm"
)

const (
	// readyLine is what the fixture's init prints once its workload ran.
	readyLine = "VIRTLE_READY:42"
	// fixtureCmdline mirrors the fixture manifests' kernel.params; virtle adds
	// the console and reboot/panic parameters itself on both backends.
	fixtureCmdline = "pci=off rdinit=/init quiet i8042.noaux i8042.nomux i8042.dumbkbd i8042.nopnp"

	readyTimeout = 30 * time.Second
	testMemory   = 128 * units.Mebibyte
)

// fixture locates the fast fixture and the VMM binaries from the
// environment the e2e-api flake check sets.
type fixture struct {
	dir         string
	qemu        string
	firecracker string
}

func loadFixture(t *testing.T) fixture {
	t.Helper()
	f := fixture{
		dir:         os.Getenv("VIRTLE_E2E_FIXTURE"),
		qemu:        os.Getenv("VIRTLE_E2E_QEMU"),
		firecracker: os.Getenv("VIRTLE_E2E_FIRECRACKER"),
	}
	if f.dir == "" || f.qemu == "" || f.firecracker == "" {
		t.Skip("requires VIRTLE_E2E_FIXTURE, VIRTLE_E2E_QEMU and VIRTLE_E2E_FIRECRACKER (Linux x86_64 with KVM)")
	}
	for _, name := range []string{"vmlinux", "bzImage", "initrd"} {
		if _, err := os.Stat(filepath.Join(f.dir, name)); err != nil {
			t.Fatalf("fixture: %v", err)
		}
	}
	return f
}

func (f fixture) path(name string) string { return filepath.Join(f.dir, name) }

// guest is one backend under test: how to construct it with its console
// wired to w, and the baseline Spec that boots the fixture on it.
type guest struct {
	name       string
	newBackend func(console io.Writer) backend.Backend
	spec       func(t *testing.T) *vm.Spec
}

func (f fixture) guests() []guest {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	spec := func(kernel string) func(t *testing.T) *vm.Spec {
		return func(t *testing.T) *vm.Spec {
			return &vm.Spec{
				CPUs:   1,
				Memory: testMemory,
				Kernel: vm.Kernel{Path: kernel, Initrd: f.path("initrd"), Cmdline: fixtureCmdline},
				Dir:    t.TempDir(),
			}
		}
	}
	return []guest{
		{
			name: "qemu",
			newBackend: func(console io.Writer) backend.Backend {
				return &qemu.Backend{
					Binary:      f.qemu,
					MachineType: "microvm",
					MachineOptions: map[string]string{
						"acpi": "off", "pcie": "off", "pit": "off", "pic": "off",
						"rtc": "off", "usb": "off", "x-option-roms": "off",
					},
					Accel:         qemu.AccelKVM,
					Console:       qemu.ConsolePrint,
					ConsoleOutput: console,
					DisableVSock:  true,
					Logger:        logger,
				}
			},
			spec: spec(f.path("bzImage")),
		},
		{
			name: "firecracker",
			newBackend: func(console io.Writer) backend.Backend {
				return &firecracker.Backend{
					Binary:        f.firecracker,
					Console:       firecracker.ConsolePrint,
					ConsoleOutput: console,
					Logger:        logger,
				}
			},
			spec: spec(f.path("vmlinux")),
		},
	}
}

// consoleLog collects guest console output and reports the first complete
// line equal to a marker. It is what a consumer has to write today to learn
// that a guest is ready: the backends expose the console only as an
// io.Writer configured before Start.
type consoleLog struct {
	marker string

	mu    sync.Mutex
	buf   bytes.Buffer
	seen  bool
	ready chan struct{}
}

func newConsoleLog(marker string) *consoleLog {
	return &consoleLog{marker: marker, ready: make(chan struct{})}
}

func (c *consoleLog) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf.Write(p)
	if !c.seen && c.hasLine(c.marker) {
		c.seen = true
		close(c.ready)
	}
	return len(p), nil
}

// hasLine reports whether a complete console line equals line; callers hold mu.
func (c *consoleLog) hasLine(line string) bool {
	text := strings.ReplaceAll(c.buf.String(), "\r\n", "\n")
	for _, l := range strings.Split(text, "\n") {
		if strings.TrimSpace(l) == line {
			return true
		}
	}
	return false
}

func (c *consoleLog) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// waitReady blocks until the marker line arrived, m exited, or the deadline
// passed.
func (c *consoleLog) waitReady(ctx context.Context, m backend.Machine, timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-c.ready:
		return nil
	case <-m.Done():
		return fmt.Errorf("guest exited before printing %q: %v", c.marker, m.Err())
	case <-timer.C:
		return fmt.Errorf("guest did not print %q within %s", c.marker, timeout)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// readyBackend starts machines through g and hands them out only once the
// guest printed the readiness line, so subtests that act immediately (a
// Shutdown right after Start) meet a booted guest. Each Start gets its own
// backend value because the console writer is per-backend, not per-machine.
type readyBackend struct {
	guest   guest
	console *consoleLog
}

func (b *readyBackend) Start(ctx context.Context, spec *vm.Spec) (backend.Machine, error) {
	console := newConsoleLog(readyLine)
	m, err := b.guest.newBackend(console).Start(ctx, spec)
	if err != nil {
		return nil, err
	}
	if err := console.waitReady(ctx, m, readyTimeout); err != nil {
		_ = m.Kill()
		return nil, fmt.Errorf("%w\n--- console ---\n%s", err, console.String())
	}
	b.console = console
	return m, nil
}

// startReady boots the fixture on g and fails the test with the console on
// any problem. The machine is killed when the test ends.
func startReady(t *testing.T, g guest, spec *vm.Spec) (backend.Machine, *consoleLog) {
	t.Helper()
	b := &readyBackend{guest: g}
	m, err := b.Start(context.Background(), spec)
	if err != nil {
		t.Fatalf("start %s: %v", g.name, err)
	}
	t.Cleanup(func() { _ = m.Kill() })
	return m, b.console
}

func waitExit(t *testing.T, m backend.Machine) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), readyTimeout)
	defer cancel()
	select {
	case <-m.Done():
	case <-ctx.Done():
		t.Fatal("machine did not exit")
	}
}

// TestConformance runs the shared backend contract against each real VMM,
// the same suite the fake-VMM and in-memory backends run.
func TestConformance(t *testing.T) {
	f := loadFixture(t)
	for _, g := range f.guests() {
		t.Run(g.name, func(t *testing.T) {
			backendtest.TestBackend(t, func(t *testing.T) (backend.Backend, *vm.Spec) {
				return &readyBackend{guest: g}, g.spec(t)
			})
		})
	}
}

// TestRootDisk boots the fixture from a raw ext4 root image instead of the
// initrd, on both backends from one Spec: the disk mounted at "/" is the
// root device and virtle passes root= for it.
func TestRootDisk(t *testing.T) {
	f := loadFixture(t)
	rootDisk := func(t *testing.T, g guest) *vm.Spec {
		spec := g.spec(t)
		spec.Kernel.Initrd = ""
		spec.Kernel.Cmdline = strings.Replace(fixtureCmdline, "rdinit=/init", "init=/init", 1)
		spec.Disks = []vm.Disk{{Path: f.path("rootfs.ext4"), Format: "raw", ReadOnly: true, GuestPath: "/"}}
		return spec
	}
	for _, g := range f.guests() {
		t.Run(g.name, func(t *testing.T) {
			startReady(t, g, rootDisk(t, g))
		})
		t.Run(g.name+"/needs a root device", func(t *testing.T) {
			spec := rootDisk(t, g)
			spec.Disks[0].GuestPath = ""
			m, err := g.newBackend(io.Discard).Start(context.Background(), spec)
			if err == nil {
				_ = m.Kill()
				t.Fatal("a disk boot without an initrd or root device was accepted")
			}
			if !strings.Contains(err.Error(), "root device") {
				t.Fatalf("error = %v, want it to name the missing root device", err)
			}
		})
	}
}

// TestScratchDisk attaches a disk that does not exist yet: virtle creates it
// as an empty ext4 image on both backends, the guest leaves its result on
// it, and the host reads that back after the machine exits.
func TestScratchDisk(t *testing.T) {
	f := loadFixture(t)
	debugfs, err := exec.LookPath("debugfs")
	if err != nil {
		t.Skip("debugfs (e2fsprogs) is required to read the image back")
	}
	for _, g := range f.guests() {
		t.Run(g.name, func(t *testing.T) {
			spec := g.spec(t)
			image := filepath.Join(spec.Dir, "scratch.img")
			spec.Disks = []vm.Disk{{Path: image, Format: "raw", Size: 256 * units.Mebibyte}}
			m, _ := startReady(t, g, spec)
			if err := m.Kill(); err != nil {
				t.Fatalf("kill: %v", err)
			}
			waitExit(t, m)
			out, err := exec.Command(debugfs, "-R", "cat /result", image).Output()
			if err != nil {
				t.Fatalf("read result back from %s: %v", image, err)
			}
			if got := strings.TrimSpace(string(out)); got != "42" {
				t.Fatalf("result on the scratch disk = %q, want 42", got)
			}
		})
	}
}

// TestDirContract covers vm.Spec.Dir on real machines: without a Dir the
// process working directory stays untouched and the private state directory
// is gone once the machine exits; with a Dir the state persists in .virtle.
func TestDirContract(t *testing.T) {
	f := loadFixture(t)
	for _, g := range f.guests() {
		t.Run(g.name+"/ephemeral", func(t *testing.T) {
			cwd := t.TempDir()
			t.Chdir(cwd)
			spec := g.spec(t)
			spec.Dir = ""
			m, _ := startReady(t, g, spec)
			status, err := m.(backend.StatusReporter).Status(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			state := filepath.Dir(status.Paths.ControlSocket)
			if filepath.Dir(state) == cwd || strings.HasPrefix(state, cwd) {
				t.Fatalf("state directory %q landed in the process working directory", state)
			}
			if err := m.Kill(); err != nil {
				t.Fatalf("kill: %v", err)
			}
			waitExit(t, m)
			if _, err := os.Stat(state); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("temporary state directory %q survived exit: %v", state, err)
			}
			if entries, err := os.ReadDir(cwd); err != nil || len(entries) != 0 {
				t.Fatalf("working directory not left alone: %v %v", entries, err)
			}
		})
		t.Run(g.name+"/durable", func(t *testing.T) {
			spec := g.spec(t)
			m, _ := startReady(t, g, spec)
			if err := m.Kill(); err != nil {
				t.Fatalf("kill: %v", err)
			}
			waitExit(t, m)
			if _, err := os.Stat(filepath.Join(spec.Dir, ".virtle", "virtle.lock")); err != nil {
				t.Fatalf("state directory did not persist under Dir: %v", err)
			}
		})
	}
}
