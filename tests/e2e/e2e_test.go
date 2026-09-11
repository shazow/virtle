//go:build integration

package e2e

import (
	"bufio"
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
	"github.com/shazow/virtle/backend/cloudhypervisor"
	"github.com/shazow/virtle/backend/firecracker"
	"github.com/shazow/virtle/backend/qemu"
	"github.com/shazow/virtle/manifest"
	"github.com/shazow/virtle/units"
	"github.com/shazow/virtle/vm"
)

const (
	// readyLine is what the fixture's init prints once its workload ran.
	readyLine = "VIRTLE_READY:42"
	// fixtureCmdline mirrors the fixture manifests' kernel.params; virtle adds
	// the console parameters itself on every backend, and its reboot/panic
	// policy on QEMU and Firecracker (Cloud Hypervisor has none). The
	// MMIO loaders skip the PCI probe and leave ACPI alone (on Firecracker's
	// tables the Ctrl-Alt-Del shutdown stopped ending the VMM); Cloud
	// Hypervisor's devices are PCI, and so are virtio-fs shares on QEMU,
	// hence pciCmdline.
	pciCmdline     = "rdinit=/init quiet i8042.noaux i8042.nomux i8042.dumbkbd i8042.nopnp"
	fixtureCmdline = "pci=off acpi=off " + pciCmdline

	readyTimeout = 30 * time.Second
	testMemory   = 128 * units.Mebibyte
)

// fixture locates the fast fixture and the VMM binaries from the
// environment the e2e-api flake check sets.
type fixture struct {
	dir             string
	qemu            string
	firecracker     string
	cloudHypervisor string
	qemuImg         string // qemu-img, for the qcow2 scenario; optional
}

func loadFixture(t *testing.T) fixture {
	t.Helper()
	f := fixture{
		dir:             os.Getenv("VIRTLE_E2E_FIXTURE"),
		qemu:            os.Getenv("VIRTLE_E2E_QEMU"),
		firecracker:     os.Getenv("VIRTLE_E2E_FIRECRACKER"),
		cloudHypervisor: os.Getenv("VIRTLE_E2E_CLOUD_HYPERVISOR"),
		qemuImg:         os.Getenv("VIRTLE_E2E_QEMU_IMG"),
	}
	if f.dir == "" || f.qemu == "" || f.firecracker == "" || f.cloudHypervisor == "" {
		skipOrFail(t, "requires VIRTLE_E2E_FIXTURE, VIRTLE_E2E_QEMU, VIRTLE_E2E_FIRECRACKER and VIRTLE_E2E_CLOUD_HYPERVISOR (Linux x86_64 with KVM)")
	}
	for _, name := range []string{"vmlinux", "bzImage", "initrd", "rootfs.ext4"} {
		if _, err := os.Stat(filepath.Join(f.dir, name)); err != nil {
			t.Fatalf("fixture: %v", err)
		}
	}
	return f
}

// skipOrFail skips the test unless VIRTLE_E2E_REQUIRED is set, as it is in
// the e2e-api flake check: there a scenario that cannot run must fail the
// check rather than let it pass without booting anything.
func skipOrFail(t *testing.T, reason string) {
	t.Helper()
	if os.Getenv("VIRTLE_E2E_REQUIRED") != "" {
		t.Fatalf("%s (VIRTLE_E2E_REQUIRED forbids skipping)", reason)
	}
	t.Skip(reason)
}

func (f fixture) path(name string) string { return filepath.Join(f.dir, name) }

// guest is one backend under test: how to construct it with its console
// wired to a writer, the kernel command line the fixture boots with on it, and the
// baseline Spec that boots the fixture on it. shareBackend, when set,
// constructs the backend for a virtio-fs share (QEMU's microvm needs ACPI
// for its PCIe bus, which the plain guest turns off); nil means the backend
// has no shares.
type guest struct {
	name         string
	newBackend   func(console io.Writer) backend.Backend
	shareBackend func(console io.Writer) backend.Backend
	cmdline      string
	spec         func(t *testing.T) *vm.Spec
}

func (f fixture) guests() []guest {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	spec := func(kernel, cmdline string) func(t *testing.T) *vm.Spec {
		return func(t *testing.T) *vm.Spec {
			return &vm.Spec{
				CPUs:   1,
				Memory: testMemory,
				Kernel: vm.Kernel{Path: kernel, Initrd: f.path("initrd"), Cmdline: cmdline},
				Dir:    t.TempDir(),
			}
		}
	}
	// QEMU's microvm with everything the fixture does not need turned off;
	// acpi is the caller's, since the PCIe bus a share rides on needs it.
	qemuBackend := func(acpi string) func(console io.Writer) backend.Backend {
		return func(console io.Writer) backend.Backend {
			accel := qemu.AccelKVM
			options := map[string]string{
				"acpi": acpi, "pit": "off", "pic": "off", "rtc": "off", "usb": "off",
			}
			if os.Getenv("VIRTLE_E2E_ACCEL") == "tcg" {
				// Without KVM there is no kvm-clock, so the guest needs
				// the PIT to calibrate its clock. Slow, for development
				// on hosts without KVM; CI runs with KVM.
				accel = qemu.AccelTCG
				options = map[string]string{"acpi": acpi, "usb": "off"}
			}
			if acpi == "off" {
				// qboot, the firmware of the ACPI-less microvm, takes
				// -kernel from fw_cfg; with ACPI on the microvm boots
				// through SeaBIOS, which needs the linuxboot option ROM
				// for it. With a share, virtle turns the PCIe bus on itself.
				options["x-option-roms"] = "off"
				options["pcie"] = "off"
			}
			return &qemu.Backend{
				Binary:         f.qemu,
				MachineType:    "microvm",
				MachineOptions: options,
				Accel:          accel,
				Console:        qemu.ConsolePrint,
				ConsoleOutput:  console,
				DisableVSock:   true,
				Logger:         logger,
			}
		}
	}
	cloudHypervisor := func(console io.Writer) backend.Backend {
		return &cloudhypervisor.Backend{
			Binary:        f.cloudHypervisor,
			Console:       cloudhypervisor.ConsolePrint,
			ConsoleOutput: console,
			Logger:        logger,
		}
	}
	return []guest{
		{
			name:         "qemu",
			newBackend:   qemuBackend("off"),
			shareBackend: qemuBackend("on"),
			cmdline:      fixtureCmdline,
			spec:         spec(f.path("bzImage"), fixtureCmdline),
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
			cmdline: fixtureCmdline,
			spec:    spec(f.path("vmlinux"), fixtureCmdline),
		},
		{
			name:         "cloud-hypervisor",
			newBackend:   cloudHypervisor,
			shareBackend: cloudHypervisor,
			cmdline:      pciCmdline,
			spec:         spec(f.path("vmlinux"), pciCmdline),
		},
	}
}

// consoleLog keeps everything the guest printed, for the failure report; it
// is the Backend's ConsoleOutput writer.
type consoleLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *consoleLog) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

// String returns the console so far with the serial line's carriage returns
// removed: the CI log renderer blanks a line that ends in one.
func (c *consoleLog) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.ReplaceAll(c.buf.String(), "\r", "")
}

// waitForLine reads console lines from term until one equals want. It gives
// up when m exits, the timeout passes, or ctx ends; the caller closes term,
// which also ends the reader.
func waitForLine(ctx context.Context, term io.Reader, want string, m backend.Machine, timeout time.Duration) error {
	found := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(term)
		for scanner.Scan() {
			if strings.TrimSpace(scanner.Text()) == want {
				close(found)
				return
			}
		}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-found:
		return nil
	case <-m.Done():
		return fmt.Errorf("guest exited before printing %q: %v", want, m.Err())
	case <-timer.C:
		return fmt.Errorf("guest did not print %q within %s", want, timeout)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// readyBackend starts machines through g and hands them out only once the
// guest printed the readiness line, so subtests that act immediately (a
// Shutdown right after Start) meet a booted guest. Readiness is read from
// Machine.Console, which replays what the guest printed before the attach,
// so it cannot be missed. Each Start gets its own backend value because the
// diagnostic ConsoleOutput writer is per-backend; a backend supplied by the
// caller (one loaded from a manifest) is used as is, and its console is
// recorded from the Term instead.
type readyBackend struct {
	guest   guest
	backend backend.Backend // when set, started instead of guest.newBackend
	console *consoleLog
}

func (b *readyBackend) Start(ctx context.Context, spec *vm.Spec) (backend.Machine, error) {
	log := &consoleLog{}
	inner := b.backend
	if inner == nil {
		inner = b.guest.newBackend(log)
	}
	m, err := inner.Start(ctx, spec)
	if err != nil {
		return nil, err
	}
	provider, ok := m.(backend.ConsoleProvider)
	if !ok {
		_ = m.Kill()
		return nil, fmt.Errorf("%s machine offers no console to read readiness from", b.guest.name)
	}
	term, err := provider.Console(ctx)
	if err != nil {
		_ = m.Kill()
		return nil, err
	}
	defer term.Close()
	var console io.Reader = term
	if b.backend != nil {
		console = io.TeeReader(term, log)
	}
	if err := waitForLine(ctx, console, readyLine, m, readyTimeout); err != nil {
		_ = m.Kill()
		return nil, fmt.Errorf("%w\n--- console ---\n%s", err, log.String())
	}
	b.console = log
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

// stepTimer logs how long each step of a scenario took since the previous
// one, so a stall shows where it happened in the CI log.
func stepTimer(t *testing.T) func(string) {
	last := time.Now()
	return func(step string) {
		t.Helper()
		now := time.Now()
		t.Logf("%s: %.1fs", step, now.Sub(last).Seconds())
		last = now
	}
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
// initrd, on every backend from one Spec: the disk mounted at "/" is the
// root device and virtle passes root= for it.
func TestRootDisk(t *testing.T) {
	f := loadFixture(t)
	rootDisk := func(t *testing.T, g guest) *vm.Spec {
		spec := g.spec(t)
		spec.Kernel.Initrd = ""
		spec.Kernel.Cmdline = strings.Replace(g.cmdline, "rdinit=/init", "init=/init", 1)
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

// manifest renders the manifest virtle launch would use for guest g: the
// fixture's CLI settings with the raw root image mounted at "/" instead of
// the initrd, working in dir.
func (f fixture) manifest(g guest, dir string) string {
	name := g.name
	params := strings.Fields(strings.Replace(g.cmdline, "rdinit=/init", "init=/init", 1))
	for i, p := range params {
		params[i] = fmt.Sprintf("%q", p)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "backend = %q\nworking_dir = %q\nnetworks = []\n[machine]\nvcpu = 1\nmemory = 128\n", name, dir)
	kernel := f.path("vmlinux")
	if name == "qemu" {
		kernel = f.path("bzImage")
		b.WriteString("type = \"microvm\"\nkvm = true\n")
	}
	fmt.Fprintf(&b, "[kernel]\npath = %q\nserial = \"print\"\nparams = [%s]\n", kernel, strings.Join(params, ", "))
	fmt.Fprintf(&b, "[[mounts]]\ntype = \"image\"\nsource = %q\ntarget = \"/\"\nread_only = true\n", f.path("rootfs.ext4"))
	switch name {
	case "qemu":
		fmt.Fprintf(&b, "[qemu]\nexec = [%q]\n", f.qemu)
		b.WriteString(`machine_options = { accel = "kvm", acpi = "off", pcie = "off", pit = "off", pic = "off", rtc = "off", usb = "off", x-option-roms = "off" }` + "\n[vsock]\nenabled = false\n")
	case "firecracker":
		fmt.Fprintf(&b, "[firecracker]\nbinary = %q\n", f.firecracker)
	case "cloud-hypervisor":
		fmt.Fprintf(&b, "[cloud-hypervisor]\nbinary = %q\n", f.cloudHypervisor)
	}
	return b.String()
}

// TestManifestRootDisk boots the raw root image the way virtle launch does:
// the manifest is lowered to a Spec and a Backend by the public manifest
// package, and the backend trusts that Spec at Start. It guards the manifest
// path of the root-device rule, which the Spec-only tests cannot see.
func TestManifestRootDisk(t *testing.T) {
	f := loadFixture(t)
	for _, g := range f.guests() {
		t.Run(g.name, func(t *testing.T) {
			spec, b, err := manifest.Load(strings.NewReader(f.manifest(g, t.TempDir())))
			if err != nil {
				t.Fatalf("load manifest: %v", err)
			}
			if len(spec.Disks) != 1 || spec.Disks[0].GuestPath != "/" {
				t.Fatalf("Disks = %+v, want the root image at /", spec.Disks)
			}
			ready := &readyBackend{guest: g, backend: b}
			m, err := ready.Start(context.Background(), spec)
			if err != nil {
				t.Fatalf("start %s from its manifest: %v", g.name, err)
			}
			t.Cleanup(func() { _ = m.Kill() })
		})
	}
}

// TestQcow2RootDisk boots the root image converted to qcow2 on the backends
// whose VMM reads that format (QEMU, Cloud Hypervisor); Firecracker refuses
// it before starting anything.
func TestQcow2RootDisk(t *testing.T) {
	f := loadFixture(t)
	if f.qemuImg == "" {
		skipOrFail(t, "requires VIRTLE_E2E_QEMU_IMG (qemu-img) to convert the root image")
	}
	image := filepath.Join(t.TempDir(), "rootfs.qcow2")
	if out, err := exec.Command(f.qemuImg, "convert", "-f", "raw", "-O", "qcow2", f.path("rootfs.ext4"), image).CombinedOutput(); err != nil {
		t.Fatalf("qemu-img convert: %v\n%s", err, out)
	}
	for _, g := range f.guests() {
		t.Run(g.name, func(t *testing.T) {
			spec := g.spec(t)
			spec.Kernel.Initrd = ""
			spec.Kernel.Cmdline = strings.Replace(g.cmdline, "rdinit=/init", "init=/init", 1)
			spec.Disks = []vm.Disk{{Path: image, Format: "qcow2", ReadOnly: true, GuestPath: "/"}}
			if g.name == "firecracker" {
				m, err := g.newBackend(io.Discard).Start(context.Background(), spec)
				if err == nil {
					_ = m.Kill()
					t.Fatal("Firecracker accepted a qcow2 image")
				}
				if !errors.Is(err, errors.ErrUnsupported) {
					t.Fatalf("error = %v, want ErrUnsupported", err)
				}
				return
			}
			startReady(t, g, spec)
		})
	}
}

// TestScratchDisk attaches a disk that does not exist yet: virtle creates it
// as an empty ext4 image on every backend, the guest leaves its result on
// it, and the host reads that back after the machine exits.
func TestScratchDisk(t *testing.T) {
	f := loadFixture(t)
	debugfs, err := exec.LookPath("debugfs")
	if err != nil {
		skipOrFail(t, "debugfs (e2fsprogs) is required to read the image back")
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

// TestConsole drives the guest's shell over backend.ConsoleProvider on every
// backend: a Term attached after boot replays the boot log, carries input
// to the console, and has no window or exit status of its own.
func TestConsole(t *testing.T) {
	f := loadFixture(t)
	for _, g := range f.guests() {
		t.Run(g.name, func(t *testing.T) {
			m, log := startReady(t, g, g.spec(t))
			term, err := m.(backend.ConsoleProvider).Console(context.Background())
			if err != nil {
				t.Fatalf("Console: %v", err)
			}
			defer term.Close()
			if err := term.Resize(80, 24); !errors.Is(err, errors.ErrUnsupported) {
				t.Fatalf("Resize = %v, want ErrUnsupported", err)
			}
			if _, err := term.Wait(context.Background()); !errors.Is(err, errors.ErrUnsupported) {
				t.Fatalf("Wait = %v, want ErrUnsupported", err)
			}
			// BusyBox init offers the console shell once Enter arrives; the
			// command line queued behind it is the shell's first input.
			if _, err := io.WriteString(term, "\necho VIRTLE_SHELL:$((6*7))\n"); err != nil {
				t.Fatalf("write to console: %v", err)
			}
			if err := waitForLine(context.Background(), term, "VIRTLE_SHELL:42", m, readyTimeout); err != nil {
				t.Fatalf("%v\n--- console ---\n%s", err, log.String())
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

// TestGuestShutdown covers Shutdown where the request goes through the
// guest: Firecracker's Ctrl-Alt-Del and Cloud Hypervisor's power button both
// reach the fixture's shutdown script, which prints a marker before the
// machine ends. QEMU's Shutdown quits the VMM from the host and has no
// marker to check.
func TestGuestShutdown(t *testing.T) {
	f := loadFixture(t)
	for _, g := range f.guests() {
		if g.name == "qemu" {
			continue
		}
		t.Run(g.name, func(t *testing.T) {
			m, log := startReady(t, g, g.spec(t))
			ctx, cancel := context.WithTimeout(context.Background(), readyTimeout)
			defer cancel()
			if err := m.Shutdown(ctx); err != nil {
				t.Fatalf("shutdown: %v\n--- console ---\n%s", err, log.String())
			}
			if !strings.Contains(log.String(), "VIRTLE_SHUTDOWN:done") {
				t.Fatalf("guest did not run its shutdown script\n--- console ---\n%s", log.String())
			}
		})
	}
}

// TestShares covers vm.Spec.Shares on real machines: virtle starts a
// virtiofsd for the share, the guest mounts it by tag and reads the file the
// host put there, and the daemon and its socket go away with the machine.
// A backend without shares says so at Start instead of dropping them.
func TestShares(t *testing.T) {
	f := loadFixture(t)
	if _, err := exec.LookPath("virtiofsd"); err != nil {
		skipOrFail(t, "virtiofsd is required on PATH")
	}
	for _, g := range f.guests() {
		t.Run(g.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "hello"), []byte("hello\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			spec := g.spec(t)
			spec.Shares = []vm.Share{{Tag: "share", HostPath: dir, GuestPath: "/mnt"}}
			if g.shareBackend == nil {
				m, err := g.newBackend(io.Discard).Start(context.Background(), spec)
				if err == nil {
					_ = m.Kill()
					t.Fatal("a backend without shares accepted one")
				}
				if !errors.Is(err, errors.ErrUnsupported) {
					t.Fatalf("error = %v, want it to wrap errors.ErrUnsupported", err)
				}
				return
			}
			// The share is a PCI device on every backend, and the guest
			// mounts it when told its tag. The kernel log stays on: this is
			// the one boot whose PCI bus differs per VMM.
			spec.Kernel.Cmdline = strings.ReplaceAll(pciCmdline, " quiet", "") + " virtle.share=share"
			shared := g
			shared.newBackend = g.shareBackend
			m, log := startReady(t, shared, spec)
			if !strings.Contains(log.String(), "VIRTLE_SHARE:hello") {
				t.Fatalf("guest did not read the share\n--- console ---\n%s", log.String())
			}
			if err := m.Kill(); err != nil {
				t.Fatalf("kill: %v", err)
			}
			waitExit(t, m)
			socket := filepath.Join(spec.Dir, ".virtle", "share.sock")
			if _, err := os.Lstat(socket); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("share socket %s after exit: %v", socket, err)
			}
		})
	}
}
