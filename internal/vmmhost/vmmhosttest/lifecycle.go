//go:build linux

// Package vmmhosttest exercises the lifecycle shared by API-driven VMMs.
// Backend tests supply real child processes implementing their own API,
// while this package checks process ownership, cancellation, and cleanup.
package vmmhosttest

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
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

const (
	testTimeout  = 5 * time.Second
	shortTimeout = 100 * time.Millisecond
)

// Options selects a child process scenario and its launch settings.
// Mode is normal, exit (status 17), diagnostic ("KVM unavailable test" on
// stderr), reject ("test rejection" from configuration), hung-api,
// ignore-shutdown, no-socket (prints a readiness marker), or missing-binary.
// ConsoleOutput, when set, enables the backend's print console.
// Other zero values keep the factory's normal launch settings.
type Options struct {
	Mode            string
	Binary          string
	StartupTimeout  time.Duration
	ShutdownTimeout time.Duration
	ConsoleOutput   io.Writer
}

// Factory constructs a backend and bootable specification in a fresh test
// directory. Every call selects its child scenario, including calls that
// restore normal behavior after a failed launch.
type Factory func(*testing.T, Options) (backend.Backend, *vm.Spec)

// TestLifecycle checks the common backend contract and the shared host
// lifecycle against the factory's child processes. It deliberately keeps
// each backend's configuration protocol and guest devices in backend tests.
func TestLifecycle(t *testing.T, newBackend Factory) {
	t.Helper()
	s := suite{newBackend: newBackend}
	t.Run("BackendContract", func(t *testing.T) {
		backendtest.TestBackend(t, func(t *testing.T) (backend.Backend, *vm.Spec) {
			return newBackend(t, Options{Mode: "normal"})
		})
	})
	t.Run("Lifecycle", s.testLifecycle)
	t.Run("StartupRollback", s.testStartupRollback)
	t.Run("CancellationAndShutdownDeadline", s.testCancellationAndShutdownDeadline)
	t.Run("StartupCancellation", s.testStartupCancellation)
	t.Run("StateDirectory", s.testStateDirectory)
	t.Run("ControlSocket", s.testControlSocket)
	t.Run("StaleControlSocketReplaced", s.testStaleControlSocketReplaced)
	t.Run("StartupDiagnostics", s.testStartupDiagnostics)
	t.Run("EarlyProcessExitPreservesStatus", s.testEarlyProcessExitPreservesStatus)
	t.Run("ConcurrentShutdown", s.testConcurrentShutdown)
	t.Run("ShutdownTimeout", s.testShutdownTimeout)
	t.Run("CreatesMissingDiskImages", s.testCreatesMissingDiskImages)
	t.Run("DefaultStateIsEphemeral", s.testDefaultStateIsEphemeral)
	t.Run("SharesManifestLockWithOtherBackends", s.testSharesManifestLockWithOtherBackends)
}

type suite struct{ newBackend Factory }

// A child output marker makes startup cancellation independent of process
// scheduling: cancel only after the child has started and before its API binds.
type eventWriter struct {
	once  sync.Once
	ready chan struct{}
}

func (w *eventWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.ready) })
	return len(p), nil
}

func (s suite) testLifecycle(t *testing.T) {
	b, spec := s.newBackend(t, Options{Mode: "normal"})
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

func (s suite) testStartupRollback(t *testing.T) {
	for _, mode := range []string{"exit", "reject", "hung-api", "missing-binary"} {
		t.Run(mode, func(t *testing.T) {
			options := Options{Mode: mode}
			if mode == "hung-api" {
				options.StartupTimeout = shortTimeout
			}
			if mode == "missing-binary" {
				options.Binary = filepath.Join(t.TempDir(), "missing-vmm")
			}
			b, spec := s.newBackend(t, options)
			m, err := b.Start(t.Context(), spec)
			if err == nil {
				_ = m.Kill()
				t.Fatal("expected startup failure")
			}
			if mode == "reject" && !strings.Contains(err.Error(), "test rejection") {
				t.Fatalf("configuration rejection not surfaced: %v", err)
			}
			// Acquiring the same state after failed startup verifies rollback.
			b, _ = s.newBackend(t, Options{Mode: "normal"})
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

func (s suite) testCancellationAndShutdownDeadline(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			b, spec := s.newBackend(t, Options{Mode: "ignore-shutdown"})
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

func (s suite) testStartupCancellation(t *testing.T) {
	w := &eventWriter{ready: make(chan struct{})}
	b, spec := s.newBackend(t, Options{Mode: "no-socket", ConsoleOutput: w})
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

func (s suite) testStateDirectory(t *testing.T) {
	b, spec := s.newBackend(t, Options{Mode: "normal"})
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

func (s suite) testControlSocket(t *testing.T) {
	b, spec := s.newBackend(t, Options{Mode: "normal"})
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

func (s suite) testStaleControlSocketReplaced(t *testing.T) {
	b, spec := s.newBackend(t, Options{Mode: "normal"})
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

func (s suite) testStartupDiagnostics(t *testing.T) {
	b, spec := s.newBackend(t, Options{Mode: "diagnostic"})
	_, err := b.Start(t.Context(), spec)
	if err == nil || !strings.Contains(err.Error(), "KVM unavailable test") {
		t.Fatalf("lost child diagnostic: %v", err)
	}
}

func (s suite) testEarlyProcessExitPreservesStatus(t *testing.T) {
	b, spec := s.newBackend(t, Options{Mode: "exit"})
	_, err := b.Start(t.Context(), spec)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 17 {
		t.Fatalf("lost child exit status: %v", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("spontaneous exit reported as caller cancellation: %v", err)
	}
}

func (s suite) testConcurrentShutdown(t *testing.T) {
	b, spec := s.newBackend(t, Options{Mode: "normal"})
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

func (s suite) testShutdownTimeout(t *testing.T) {
	b, spec := s.newBackend(t, Options{Mode: "ignore-shutdown", ShutdownTimeout: shortTimeout})
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

func (s suite) testCreatesMissingDiskImages(t *testing.T) {
	b, spec := s.newBackend(t, Options{Mode: "normal"})
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

func (s suite) testDefaultStateIsEphemeral(t *testing.T) {
	t.Chdir(t.TempDir())
	b, spec := s.newBackend(t, Options{Mode: "normal"})
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

func (s suite) testSharesManifestLockWithOtherBackends(t *testing.T) {
	b, spec := s.newBackend(t, Options{Mode: "normal"})
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
