//go:build linux

package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/backend/backendtest"
	"github.com/shazow/virtle/internal/control"
	"github.com/shazow/virtle/units"
	"github.com/shazow/virtle/vm"
)

const testTimeout = 5 * time.Second
const shortTimeout = 100 * time.Millisecond

// TestMain doubles as a real child process speaking Firecracker's Unix HTTP
// protocol. Filesystem use here exercises the actual socket/process boundary.
func TestMain(m *testing.M) {
	if mode := os.Getenv("VIRTLE_TEST_FIRECRACKER"); mode != "" {
		if mode == "diagnostic" {
			fmt.Fprintln(os.Stderr, "KVM unavailable test")
			os.Exit(1)
		}
		if mode == "exit" {
			os.Exit(17)
		}
		if mode == "no-socket" {
			signals := make(chan os.Signal, 1)
			signal.Notify(signals, syscall.SIGTERM)
			fmt.Println("WAIT")
			<-signals
			os.Exit(0)
		}
		listener, err := net.Listen("unix", os.Args[2])
		if err != nil {
			panic(err)
		}
		stop := make(chan struct{})
		stopped := make(chan struct{})
		server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if mode == "reject" && r.URL.Path == "/boot-source" {
				http.Error(w, `{"fault_message":"test rejection"}`, 400)
				return
			}
			if mode == "hung-api" {
				<-r.Context().Done()
				return
			}
			var a action
			_ = json.NewDecoder(r.Body).Decode(&a)
			w.WriteHeader(http.StatusNoContent)
			if a.Type == "SendCtrlAltDel" && mode != "ignore-shutdown" {
				close(stop)
			}
		})}
		go func() { <-stop; _ = server.Shutdown(context.Background()); close(stopped) }()
		_ = server.Serve(listener)
		// Serve returns when Shutdown closes the listener, before in-flight
		// handlers have necessarily flushed their replies. Reap only after
		// Shutdown has drained those handlers, just as a real API server does.
		<-stopped
		os.Exit(0)
	}
	// Under -race every helper child would otherwise pause a second at exit
	// to flush race reports; the children are fakes with nothing to report.
	if os.Getenv("GORACE") == "" {
		os.Setenv("GORACE", "atexit_sleep_ms=0")
	}
	// The fake VMM honors SendCtrlAltDel on every architecture.
	ctrlAltDelSupported = true
	os.Exit(m.Run())
}

// TestBackendContract runs the shared backend conformance suite against the
// fake VMM.
func TestBackendContract(t *testing.T) {
	backendtest.TestBackend(t, func(t *testing.T) (backend.Backend, *vm.Spec) {
		b, spec := helperBackend(t, "normal")
		return b, spec
	})
}

func helperBackend(t *testing.T, mode string) (*Backend, *vm.Spec) {
	t.Helper()
	t.Setenv("VIRTLE_TEST_FIRECRACKER", mode)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return &Backend{Binary: binary, StartupTimeout: testTimeout, ShutdownTimeout: testTimeout}, &vm.Spec{Dir: t.TempDir(), Kernel: vm.Kernel{Path: "kernel"}}
}

func TestLifecycle(t *testing.T) {
	b, spec := helperBackend(t, "normal")
	m, err := b.Start(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Kill() })
	status, err := m.(backend.StatusReporter).Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if status.PID <= 0 || status.State != backend.StateReady || status.Paths.MonitorSocket == "" {
		t.Fatal(status)
	}
	if _, err := m.RemoteControl(); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatal(err)
	}
	if _, err := b.Start(t.Context(), spec); err == nil {
		t.Fatal("concurrent state ownership succeeded")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := m.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := m.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := m.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := m.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := m.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(status.PID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("child still exists: %v", err)
	}
	if _, err := os.Lstat(filepath.Dir(status.Paths.MonitorSocket)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("runtime directory remains: %v", err)
	}
	m, err = b.Start(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Kill(); err != nil {
		t.Fatal(err)
	}
}

func TestStartupRollback(t *testing.T) {
	for _, mode := range []string{"exit", "reject", "hung-api", "missing-binary"} {
		t.Run(mode, func(t *testing.T) {
			b, spec := helperBackend(t, mode)
			if mode == "hung-api" {
				b.StartupTimeout = shortTimeout // the only mode that needs the deadline
			}
			if mode == "missing-binary" {
				b.Binary = "/nonexistent/virtle-firecracker"
			}
			m, err := b.Start(t.Context(), spec)
			if err == nil {
				_ = m.Kill()
				t.Fatal("expected startup failure")
			}
			if mode == "reject" && !strings.Contains(err.Error(), "test rejection") {
				t.Fatalf("configuration rejection not surfaced: %v", err)
			}
			// Acquiring the same state after failed startup verifies rollback.
			t.Setenv("VIRTLE_TEST_FIRECRACKER", "normal")
			b.Binary, _ = os.Executable()
			b.StartupTimeout = testTimeout
			m, err = b.Start(t.Context(), spec)
			if err != nil {
				t.Fatal(err)
			}
			if err := m.Kill(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCancellationAndShutdownDeadline(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			b, spec := helperBackend(t, "ignore-shutdown")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			m, err := b.Start(ctx, spec)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = m.Kill() })
			if mode == "cancel" {
				cancel()
			} else {
				shutdownCtx, stop := context.WithCancel(t.Context())
				stop()
				if err := m.Shutdown(shutdownCtx); !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			}
			waitCtx, stop := context.WithTimeout(t.Context(), testTimeout)
			defer stop()
			if err := m.Wait(waitCtx); errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("child was not reaped")
			}
		})
	}
}

type eventWriter struct {
	once  sync.Once
	ready chan struct{}
}

func (w *eventWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.ready) })
	return len(p), nil
}

func TestStartupCancellation(t *testing.T) {
	b, spec := helperBackend(t, "no-socket")
	w := &eventWriter{ready: make(chan struct{})}
	b.Console = "print"
	b.ConsoleOutput = w
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := b.Start(ctx, spec); result <- err }()
	select {
	case <-w.ready:
		cancel()
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestStateDirectory(t *testing.T) {
	b, spec := helperBackend(t, "normal")
	state := filepath.Join(spec.Dir, ".virtle")

	// A missing state directory is created privately and holds the lock.
	m, err := b.Start(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	first := m
	t.Cleanup(func() { _ = first.Kill() })
	if info, err := os.Stat(state); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("state directory: %v %v", info, err)
	}
	if pid, err := os.ReadFile(filepath.Join(state, "virtle.lock")); err != nil || strings.TrimSpace(string(pid)) != strconv.Itoa(os.Getpid()) {
		t.Fatalf("lock file: %q %v", pid, err)
	}
	if err := m.Kill(); err != nil {
		t.Fatal(err)
	}

	// An existing directory is used as it is, whatever its mode.
	if err := os.Chmod(state, 0o755); err != nil {
		t.Fatal(err)
	}
	m, err = b.Start(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Kill() })
	if err := m.Kill(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(state); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("existing state directory changed: %v %v", info, err)
	}
}

func TestControlSocket(t *testing.T) {
	b, spec := helperBackend(t, "normal")
	m, err := b.Start(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Kill() })
	status, _ := m.(backend.StatusReporter).Status(t.Context())
	if status.Paths.ControlSocket != filepath.Join(spec.Dir, ".virtle", "virtle.sock") {
		t.Fatalf("control socket: %q", status.Paths.ControlSocket)
	}
	client, err := control.Dial(t.Context(), status.Paths.ControlSocket)
	if err != nil {
		t.Fatal(err)
	}
	remote, err := client.(backend.StatusReporter).Status(t.Context())
	if err != nil || remote.PID != status.PID {
		t.Fatalf("status %+v: %v", remote, err)
	}
	methods, err := control.Raw(t.Context(), status.Paths.ControlSocket, "methods", nil)
	if err != nil || strings.Contains(string(methods), `"suspend"`) {
		t.Fatalf("methods %s: %v", methods, err)
	}
	if err := client.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := m.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(status.Paths.ControlSocket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("control socket remains: %v", err)
	}
}

// TestStaleControlSocketReplaced covers restarting after a crash: the lock
// proves nobody owns the state directory, so a leftover socket path is
// replaced rather than reported as a conflict.
func TestStaleControlSocketReplaced(t *testing.T) {
	b, spec := helperBackend(t, "normal")
	state := filepath.Join(spec.Dir, ".virtle")
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	stale, err := net.Listen("unix", filepath.Join(state, "virtle.sock"))
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}
	m, err := b.Start(t.Context(), spec)
	if err != nil {
		t.Fatalf("stale control socket blocked start: %v", err)
	}
	t.Cleanup(func() { _ = m.Kill() })
	status, _ := m.(backend.StatusReporter).Status(t.Context())
	if _, err := control.Dial(t.Context(), status.Paths.ControlSocket); err != nil {
		t.Fatalf("control socket not serving after replacing stale path: %v", err)
	}
}

func TestStartupDiagnostics(t *testing.T) {
	b, spec := helperBackend(t, "diagnostic")
	_, err := b.Start(t.Context(), spec)
	if err == nil || !strings.Contains(err.Error(), "KVM unavailable test") {
		t.Fatalf("lost child diagnostic: %v", err)
	}
}

func TestEarlyProcessExitPreservesStatus(t *testing.T) {
	b, spec := helperBackend(t, "exit")
	_, err := b.Start(t.Context(), spec)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 17 {
		t.Fatalf("lost child exit status: %v", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("spontaneous exit reported as caller cancellation: %v", err)
	}
}

func TestConcurrentShutdown(t *testing.T) {
	b, spec := helperBackend(t, "normal")
	m, err := b.Start(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Kill() })
	results := make(chan error, 8)
	for range 8 {
		go func() { results <- m.Shutdown(t.Context()) }()
	}
	for range 8 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
}

func TestShutdownTimeout(t *testing.T) {
	b, spec := helperBackend(t, "ignore-shutdown")
	b.ShutdownTimeout = shortTimeout
	m, err := b.Start(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Kill() })
	if err := m.Shutdown(t.Context()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	select {
	case <-m.Done():
	default:
		t.Fatal("Shutdown returned without completing teardown")
	}
}

// TestCreatesMissingDiskImages covers vm.Disk.Size: a missing image is
// formatted before launch, as QEMU does, and an existing one is kept.
func TestCreatesMissingDiskImages(t *testing.T) {
	b, spec := helperBackend(t, "normal")
	image := filepath.Join(spec.Dir, "scratch.img")
	spec.Kernel.Initrd = "initrd" // the scratch disk is not the root
	spec.Disks = []vm.Disk{{Path: image, Format: "raw", Size: 256 * units.Mebibyte}}
	m, err := b.Start(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Kill() })
	info, err := os.Stat(image)
	if err != nil || info.Size() != 256<<20 {
		t.Fatalf("disk image not created before launch: %v %v", info, err)
	}
	if err := m.Kill(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(image); err != nil {
		t.Fatalf("created disk image did not survive exit: %v", err)
	}
}

// TestConsoleFollowsTheMachine covers backend.ConsoleProvider on the fake
// VMM: a print console can be attached and ends when the machine exits; no
// console means errors.ErrUnsupported.
func TestConsoleFollowsTheMachine(t *testing.T) {
	b, spec := helperBackend(t, "normal")
	b.Console, b.ConsoleOutput = ConsolePrint, io.Discard
	m, err := b.Start(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Kill() })
	term, err := m.(backend.ConsoleProvider).Console(t.Context())
	if err != nil {
		t.Fatalf("Console: %v", err)
	}
	defer term.Close()
	if err := term.Resize(80, 24); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("Resize = %v, want ErrUnsupported", err)
	}
	if err := m.Kill(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(term); err != nil {
		t.Fatalf("console did not end with EOF after exit: %v", err)
	}

	b, spec = helperBackend(t, "normal")
	m, err = b.Start(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Kill() })
	if _, err := m.(backend.ConsoleProvider).Console(t.Context()); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("Console without a serial console = %v, want ErrUnsupported", err)
	}
}

// TestShutdownWithoutCtrlAltDel covers hosts where Firecracker has no guest
// shutdown request: Shutdown still stops the VMM but names the missing
// capability.
func TestShutdownWithoutCtrlAltDel(t *testing.T) {
	ctrlAltDelSupported = false
	t.Cleanup(func() { ctrlAltDelSupported = true })
	b, spec := helperBackend(t, "normal")
	m, err := b.Start(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Kill() })
	if err := m.Shutdown(t.Context()); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("Shutdown error = %v, want ErrUnsupported", err)
	}
	select {
	case <-m.Done():
	default:
		t.Fatal("Shutdown returned without stopping the VMM")
	}
}

func TestDefaultStateIsEphemeral(t *testing.T) {
	t.Chdir(t.TempDir())
	b, spec := helperBackend(t, "normal")
	spec.Dir = ""
	m, err := b.Start(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Kill() })
	status, _ := m.(backend.StatusReporter).Status(t.Context())
	state := filepath.Dir(status.Paths.ControlSocket)
	if !strings.HasPrefix(filepath.Base(state), "virtle-state-") {
		t.Fatalf("default state not ephemeral: %s", state)
	}
	if err := m.Kill(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(state); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ephemeral state remains: %v", err)
	}
}

func TestSharesManifestLockWithQEMU(t *testing.T) {
	b, spec := helperBackend(t, "normal")
	state := filepath.Join(spec.Dir, ".virtle")
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(state, "virtle.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if m, err := b.Start(t.Context(), spec); err == nil {
		_ = m.Kill()
		t.Fatal("ignored manifest lock owned by another backend")
	}
}

var _ io.Writer = (*eventWriter)(nil)

func TestStatusReportsTAPNIC(t *testing.T) {
	b, spec := helperBackend(t, "normal")
	b.Link = TAP{Name: "tap0"}
	m, err := b.Start(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Kill() })
	status, err := m.(backend.StatusReporter).Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []backend.NetworkStatus{{ID: "microvm1", MAC: "02:02:00:00:00:01"}}
	if !reflect.DeepEqual(status.Networks, want) {
		t.Fatalf("Networks = %+v, want %+v", status.Networks, want)
	}
}
