//go:build linux

package cloudhypervisor

import (
	"bufio"
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
	"unsafe"

	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/backend/backendtest"
	"github.com/shazow/virtle/internal/control"
	imanifest "github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/units"
	"github.com/shazow/virtle/vm"
)

const testTimeout = 5 * time.Second
const shortTimeout = 100 * time.Millisecond

// TestMain doubles as two real child processes: a fake Cloud Hypervisor
// speaking its Unix HTTP API, and a fake virtiofsd that binds its socket.
// The two are told apart by their arguments, since helpers inherit the
// environment. Filesystem use here exercises the actual socket/process
// boundary.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && strings.HasPrefix(os.Args[1], "--socket-path=") {
		fakeVirtiofsd(strings.TrimPrefix(os.Args[1], "--socket-path="))
	}
	if len(os.Args) > 1 && strings.HasPrefix(os.Args[1], "--helper=") {
		fakeHelper(strings.TrimPrefix(os.Args[1], "--helper="))
	}
	if mode := os.Getenv("VIRTLE_TEST_CLOUD_HYPERVISOR"); mode != "" {
		fakeVMM(mode)
	}
	// Under -race every helper child would otherwise pause a second at exit
	// to flush race reports; the children are fakes with nothing to report.
	if os.Getenv("GORACE") == "" {
		os.Setenv("GORACE", "atexit_sleep_ms=0")
	}
	os.Exit(m.Run())
}

// fakeVMM serves Cloud Hypervisor's API on the socket named by the
// --api-socket path=<socket> argument, or misbehaves as mode says. A power
// button press ends it the way a guest power-off ends the real VMM.
func fakeVMM(mode string) {
	switch mode {
	case "diagnostic":
		fmt.Fprintln(os.Stderr, "KVM unavailable test")
		os.Exit(1)
	case "exit":
		os.Exit(17)
	case "no-socket":
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGTERM)
		fmt.Println("WAIT")
		<-signals
		os.Exit(0)
	case "sleep":
		time.Sleep(3 * time.Second)
		os.Exit(0)
	case "straggler":
		// Something the VMM spawned inherits its console and outlives it,
		// in a process group of its own so the VMM's kill does not reach it.
		child := exec.Command(os.Args[0])
		child.Env = append(os.Environ(), "VIRTLE_TEST_CLOUD_HYPERVISOR=sleep")
		child.Stdin, child.Stdout = os.Stdin, os.Stdout
		child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := child.Start(); err != nil {
			panic(err)
		}
	}
	socket := strings.TrimPrefix(os.Args[2], "path=")
	// Like the real VMM, take console input only from a terminal; echo it,
	// so a test can see its keystrokes cross the pseudo-terminal both ways.
	if stdinIsTerminal() {
		go func() { _, _ = io.Copy(os.Stdout, os.Stdin) }()
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		panic(err)
	}
	var recordMu sync.Mutex
	record := os.Getenv("VIRTLE_TEST_CLOUD_HYPERVISOR_RECORD")
	stop := make(chan struct{})
	stopped := make(chan struct{})
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if record != "" {
			line, _ := json.Marshal(map[string]any{"method": r.Method, "path": r.URL.Path, "body": json.RawMessage(nonEmpty(body))})
			recordMu.Lock()
			f, err := os.OpenFile(record, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
			if err == nil {
				_, _ = f.Write(append(line, '\n'))
				_ = f.Close()
			}
			recordMu.Unlock()
		}
		if mode == "hung-api" {
			<-r.Context().Done()
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "PUT /api/v1/vm.create":
			if mode == "reject" {
				http.Error(w, `["Error creating VM: test rejection","inner cause"]`, http.StatusBadRequest)
				return
			}
			// Like the real VMM, connect to every share's vhost-user socket
			// now and refuse the VM when one does not answer.
			var config struct {
				FS []struct {
					Socket string `json:"socket"`
				} `json:"fs"`
			}
			_ = json.Unmarshal(body, &config)
			for _, share := range config.FS {
				conn, err := net.DialTimeout("unix", share.Socket, time.Second)
				if err != nil {
					messages, _ := json.Marshal([]string{"Error creating VM: vhost-user-fs " + share.Socket, err.Error()})
					http.Error(w, string(messages), http.StatusBadRequest)
					return
				}
				_ = conn.Close()
			}
			w.WriteHeader(http.StatusNoContent)
		case "PUT /api/v1/vm.boot":
			w.WriteHeader(http.StatusNoContent)
		case "PUT /api/v1/vm.power-button":
			w.WriteHeader(http.StatusNoContent)
			if mode != "ignore-shutdown" {
				close(stop)
			}
		case "GET /api/v1/vmm.ping":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"build_version":"test","version":"test","pid":1,"features":[]}`))
		default:
			http.Error(w, "[]", http.StatusNotFound)
		}
	})}
	go func() { <-stop; _ = server.Shutdown(context.Background()); close(stopped) }()
	_ = server.Serve(listener)
	// Serve returns when Shutdown closes the listener, before in-flight
	// handlers have necessarily flushed their replies. Exit only after
	// Shutdown has drained those handlers, just as a real API server does.
	<-stopped
	os.Exit(0)
}

func nonEmpty(body []byte) []byte {
	if len(body) == 0 {
		return []byte("null")
	}
	return body
}

// fakeVirtiofsd binds the vhost-user socket (after a delay when asked, to
// prove startup waits for it), records its PID next to it, and leaves on
// SIGTERM the way virtiofsd does.
func fakeVirtiofsd(socket string) {
	switch os.Getenv("VIRTLE_TEST_VIRTIOFSD") {
	case "exit":
		fmt.Fprintln(os.Stderr, "virtiofsd test failure")
		os.Exit(3)
	case "slow":
		time.Sleep(300 * time.Millisecond)
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(socket+".pid", []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		panic(err)
	}
	if os.Getenv("VIRTLE_TEST_VIRTIOFSD") == "exit-after-bind" {
		// Dies the way a daemon does that fails after binding (a shared
		// directory it cannot serve), leaving its socket file behind.
		fmt.Fprintln(os.Stderr, "virtiofsd shared-dir test failure")
		os.Exit(4)
	}
	<-signals
	_ = listener.Close()
	os.Exit(0)
}

// fakeHelper is a [[run]] entry: it records its PID at the given path and
// leaves on SIGTERM.
func fakeHelper(pidPath string) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM)
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		panic(err)
	}
	<-signals
	os.Exit(0)
}

// TestRunHelpersFollowTheMachine covers a manifest's [[run]] entries: they
// start before the VMM, with the manifest's templates rendered, and stop
// when the machine does.
func TestRunHelpersFollowTheMachine(t *testing.T) {
	b, spec := helperBackend(t, "normal")
	doc, err := imanifest.DecodeDocumentBytes([]byte(fmt.Sprintf("backend = 'cloud-hypervisor'\n[kernel]\npath = 'kernel'\n[[run]]\nexec = [%q, '--helper={{.StateDir}}/helper.pid']\n", b.Binary)), "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewBackendFromDocument(doc, *b).Start(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Kill() })
	pidText, err := os.ReadFile(filepath.Join(spec.Dir, ".virtle", "helper.pid"))
	if err != nil {
		t.Fatalf("helper did not start before the VMM: %v", err)
	}
	pid, _ := strconv.Atoi(string(pidText))
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("helper %d while running: %v", pid, err)
	}
	if err := m.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("helper %d after exit: %v", pid, err)
	}
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
	t.Setenv("VIRTLE_TEST_CLOUD_HYPERVISOR", mode)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return &Backend{Binary: binary, StartupTimeout: testTimeout, ShutdownTimeout: testTimeout}, &vm.Spec{Dir: t.TempDir(), Kernel: vm.Kernel{Path: "kernel"}}
}

// installFakeVirtiofsd puts a virtiofsd on PATH that is this test binary in
// its fake-daemon role.
func installFakeVirtiofsd(t *testing.T, mode string) {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Symlink(binary, filepath.Join(dir, "virtiofsd")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("VIRTLE_TEST_VIRTIOFSD", mode)
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
				b.Binary = "/nonexistent/virtle-cloud-hypervisor"
			}
			m, err := b.Start(t.Context(), spec)
			if err == nil {
				_ = m.Kill()
				t.Fatal("expected startup failure")
			}
			// The VMM's message list is flattened, outermost first.
			if mode == "reject" && !strings.Contains(err.Error(), "test rejection: inner cause") {
				t.Fatalf("configuration rejection not surfaced: %v", err)
			}
			// Acquiring the same state after failed startup verifies rollback.
			t.Setenv("VIRTLE_TEST_CLOUD_HYPERVISOR", "normal")
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

// TestShutdownTimeout covers a guest that ignores the power button: the VMM
// is killed once Backend.ShutdownTimeout passes, with no other rung.
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

// stdinIsTerminal reports whether the fake VMM's standard input is a
// terminal, the only kind of input the real one reads.
func stdinIsTerminal() bool {
	var termios syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, 0, syscall.TCGETS, uintptr(unsafe.Pointer(&termios)))
	return errno == 0
}

// TestConsoleFollowsTheMachine covers backend.ConsoleProvider on the fake
// VMM: a print console can be attached, what is typed into it reaches the
// VMM through the pseudo-terminal it insists on (the fake echoes it), the
// console ends when the machine exits, and no console means
// errors.ErrUnsupported.
// TestKillOutlivesConsoleStraggler covers a process that inherited the VMM's
// console and outlives it: the reaper's bounded wait for the console to
// drain is teardown work, not a wedged VMM, so Kill waits it out and reports
// success rather than a teardown deadline.
func TestKillOutlivesConsoleStraggler(t *testing.T) {
	b, spec := helperBackend(t, "straggler")
	b.Console, b.ConsoleOutput = ConsolePrint, io.Discard
	m, err := b.Start(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Kill(); err != nil {
		t.Fatalf("Kill with a console straggler: %v", err)
	}
	select {
	case <-m.Done():
	default:
		t.Fatal("Kill returned before the machine was done")
	}
	if status, _ := m.(backend.StatusReporter).Status(context.Background()); status.State != backend.StateStopped {
		t.Fatalf("state after Kill = %q, want stopped", status.State)
	}
}

// TestInteractiveConsoleUsesTheTerminal covers ConsoleInteractive: the VMM
// inherits the process's standard input and stays in its process group, as
// QEMU's interactive console does, and no vm.Term is served.
func TestInteractiveConsoleUsesTheTerminal(t *testing.T) {
	b, spec := helperBackend(t, "normal")
	b.Console, b.ConsoleOutput = ConsoleInteractive, io.Discard
	m, err := b.Start(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Kill() })
	status, _ := m.(backend.StatusReporter).Status(t.Context())
	ours, err := os.Readlink("/proc/self/fd/0")
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/0", status.PID))
	if err != nil {
		t.Fatal(err)
	}
	if ours != theirs {
		t.Fatalf("VMM stdin = %s, want ours (%s)", theirs, ours)
	}
	if group, _ := syscall.Getpgid(status.PID); group != syscall.Getpgrp() {
		t.Fatalf("VMM process group = %d, want ours (%d)", group, syscall.Getpgrp())
	}
	if _, err := m.(backend.ConsoleProvider).Console(t.Context()); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("Console on an interactive console = %v, want ErrUnsupported", err)
	}
	if err := m.Kill(); err != nil {
		t.Fatal(err)
	}
}

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
	if _, err := io.WriteString(term, "ping\n"); err != nil {
		t.Fatalf("write to console: %v", err)
	}
	echoed := make(chan string, 1)
	go func() {
		line, err := bufio.NewReader(term).ReadString('\n')
		echoed <- line + fmt.Sprint(err)
	}()
	select {
	case line := <-echoed:
		if line != "ping\n<nil>" {
			t.Fatalf("console echoed %q", line)
		}
	case <-time.After(testTimeout):
		t.Fatal("keystrokes did not come back through the terminal")
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

func TestSharesManifestLockWithOtherBackends(t *testing.T) {
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

type recordedRequest struct {
	Method string         `json:"method"`
	Path   string         `json:"path"`
	Body   map[string]any `json:"body"`
}

func readRecord(t *testing.T, path string) []recordedRequest {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var requests []recordedRequest
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var request recordedRequest
		if err := json.Unmarshal([]byte(line), &request); err != nil {
			t.Fatalf("record line %q: %v", line, err)
		}
		requests = append(requests, request)
	}
	return requests
}

// TestSharesStartVirtiofsd covers vm.Spec.Shares: a virtiofsd per share is
// started before the VMM, the VM is created only once its socket exists
// (however slowly the daemon binds it), the VM config carries the share on
// shared memory, and teardown stops the daemon and removes its socket.
func TestSharesStartVirtiofsd(t *testing.T) {
	for _, mode := range []string{"prompt", "slow"} {
		t.Run(mode, func(t *testing.T) {
			b, spec := helperBackend(t, "normal")
			installFakeVirtiofsd(t, mode)
			record := filepath.Join(t.TempDir(), "requests.jsonl")
			t.Setenv("VIRTLE_TEST_CLOUD_HYPERVISOR_RECORD", record)
			spec.Shares = []vm.Share{{Tag: "share", HostPath: t.TempDir(), GuestPath: "/mnt"}}
			m, err := b.Start(t.Context(), spec)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = m.Kill() })
			socket := filepath.Join(spec.Dir, ".virtle", "share.sock")
			if info, err := os.Stat(socket); err != nil || info.Mode().Type() != os.ModeSocket {
				t.Fatalf("share socket while running: %v %v", info, err)
			}
			pidText, err := os.ReadFile(socket + ".pid")
			if err != nil {
				t.Fatal(err)
			}
			pid, _ := strconv.Atoi(string(pidText))
			requests := readRecord(t, record)
			if len(requests) != 2 || requests[0].Path != "/api/v1/vm.create" || requests[1].Path != "/api/v1/vm.boot" || requests[1].Method != "PUT" {
				t.Fatalf("requests = %+v", requests)
			}
			create := requests[0].Body
			if memory := create["memory"].(map[string]any); memory["shared"] != true {
				t.Fatalf("memory = %v, want shared", memory)
			}
			fs := create["fs"].([]any)
			if len(fs) != 1 {
				t.Fatalf("fs = %v", fs)
			}
			if share := fs[0].(map[string]any); share["tag"] != "share" || share["socket"] != socket || share["num_queues"] != float64(1) || share["queue_size"] != float64(1024) {
				t.Fatalf("fs[0] = %v", share)
			}
			if err := m.Kill(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(socket); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("share socket after exit: %v", err)
			}
			if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
				t.Fatalf("virtiofsd %d still exists: %v", pid, err)
			}
		})
	}
}

// TestStaleShareSocketIsReplaced covers a share socket left behind by a
// crashed launch: it is removed before the daemon starts, so the daemon can
// bind it and the VMM connects to a live socket rather than the dead file.
func TestStaleShareSocketIsReplaced(t *testing.T) {
	b, spec := helperBackend(t, "normal")
	installFakeVirtiofsd(t, "prompt")
	spec.Shares = []vm.Share{{Tag: "share", HostPath: t.TempDir()}}
	state := filepath.Join(spec.Dir, ".virtle")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	stale, err := net.Listen("unix", filepath.Join(state, "share.sock"))
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = stale.Close()
	m, err := b.Start(t.Context(), spec)
	if err != nil {
		t.Fatalf("Start over a stale share socket: %v", err)
	}
	if err := m.Kill(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(state, "share.sock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("share socket after exit: %v", err)
	}
}

// TestShareDaemonDeathAfterBindIsDiagnosed covers a virtiofsd that binds its
// socket and then dies: the VMM refuses the VM over the dead socket, and the
// failure carries the daemon's last words rather than the VMM's alone.
func TestShareDaemonDeathAfterBindIsDiagnosed(t *testing.T) {
	b, spec := helperBackend(t, "normal")
	installFakeVirtiofsd(t, "exit-after-bind")
	spec.Shares = []vm.Share{{Tag: "share", HostPath: t.TempDir()}}
	m, err := b.Start(t.Context(), spec)
	if err == nil {
		_ = m.Kill()
		t.Fatal("expected startup failure")
	}
	for _, want := range []string{"vm.create", "virtiofsd shared-dir test failure"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q lacks %q", err, want)
		}
	}
}

// TestShareDaemonExitAbortsStartup covers a virtiofsd that dies before
// binding its socket: startup fails with its stderr instead of waiting out
// the deadline, and the state is released for the next launch.
func TestShareDaemonExitAbortsStartup(t *testing.T) {
	b, spec := helperBackend(t, "normal")
	installFakeVirtiofsd(t, "exit")
	spec.Shares = []vm.Share{{Tag: "share", HostPath: t.TempDir()}}
	start := time.Now()
	m, err := b.Start(t.Context(), spec)
	if err == nil {
		_ = m.Kill()
		t.Fatal("expected startup failure")
	}
	if !strings.Contains(err.Error(), "virtiofsd test failure") || !strings.Contains(err.Error(), "exited") {
		t.Fatalf("lost daemon diagnostic: %v", err)
	}
	if time.Since(start) > testTimeout/2 {
		t.Fatalf("startup waited out the deadline instead of noticing the exit: %v", time.Since(start))
	}
	t.Setenv("VIRTLE_TEST_VIRTIOFSD", "prompt")
	m, err = b.Start(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Kill(); err != nil {
		t.Fatal(err)
	}
}
