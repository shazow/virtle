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
	"os/signal"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/shazow/virtle/backend"
	imanifest "github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/internal/vmmhost/vmmhosttest"
	"github.com/shazow/virtle/vm"
)

const testTimeout = 5 * time.Second

// TestMain doubles as a real child process speaking Firecracker's Unix HTTP
// protocol. Filesystem use here exercises the actual socket/process boundary.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && strings.HasPrefix(os.Args[1], "--helper=") {
		// A [[run]] entry: record the PID at the given path, leave on SIGTERM.
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGTERM)
		if err := os.WriteFile(strings.TrimPrefix(os.Args[1], "--helper="), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			panic(err)
		}
		<-signals
		os.Exit(0)
	}
	if mode := os.Getenv("VIRTLE_TEST_FIRECRACKER"); mode != "" {
		if path := os.Getenv("VIRTLE_TEST_VMM_ARGV"); path != "" {
			_ = os.WriteFile(path, []byte(strings.Join(os.Args[1:], "\n")), 0o600)
		}
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

func TestLifecycle(t *testing.T) {
	vmmhosttest.TestLifecycle(t, func(t *testing.T, options vmmhosttest.Options) (backend.Backend, *vm.Spec) {
		b, spec := helperBackend(t, options.Mode)
		if options.Binary != "" {
			b.Binary = options.Binary
		}
		if options.StartupTimeout != 0 {
			b.StartupTimeout = options.StartupTimeout
		}
		if options.ShutdownTimeout != 0 {
			b.ShutdownTimeout = options.ShutdownTimeout
		}
		if options.ConsoleOutput != nil {
			b.Console, b.ConsoleOutput = ConsolePrint, options.ConsoleOutput
		}
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

// TestExtraArgsReachTheVMM covers Backend.ExtraArgs: they follow virtle's
// own arguments on the VMM's command line.
func TestExtraArgsReachTheVMM(t *testing.T) {
	b, spec := helperBackend(t, "normal")
	b.ExtraArgs = []string{"--log-path", "/dev/null"}
	argv := filepath.Join(t.TempDir(), "argv")
	t.Setenv("VIRTLE_TEST_VMM_ARGV", argv)
	m, err := b.Start(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Kill() }()
	got, err := os.ReadFile(argv)
	if err != nil {
		t.Fatal(err)
	}
	if args := strings.Split(string(got), "\n"); len(args) != 4 || args[0] != "--api-sock" || args[2] != "--log-path" || args[3] != "/dev/null" {
		t.Fatalf("VMM argv = %q", args)
	}
}

// TestRunHelpersFollowTheMachine covers a manifest's [[run]] entries: they
// start before the VMM, with the manifest's templates rendered, and stop
// when the machine does.
func TestRunHelpersFollowTheMachine(t *testing.T) {
	b, spec := helperBackend(t, "normal")
	doc, err := imanifest.DecodeDocumentBytes([]byte(fmt.Sprintf("backend = 'firecracker'\n[kernel]\npath = 'kernel'\n[[run]]\nexec = [%q, '--helper={{.StateDir}}/helper.pid']\n", b.Binary)), "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewBackendFromDocument(doc, *b).Start(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Kill() })
	// The helper is started before the VMM, but it records its PID only
	// once it runs, and the fake VMM boots faster than a Go binary starts.
	pidPath := filepath.Join(spec.Dir, ".virtle", "helper.pid")
	var pidText []byte
	for deadline := time.Now().Add(testTimeout); ; time.Sleep(10 * time.Millisecond) {
		if pidText, err = os.ReadFile(pidPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("helper did not start: %v", err)
		}
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
