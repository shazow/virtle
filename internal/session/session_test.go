package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/backend/backendtest"
	"github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/internal/sessionbridge"
	"github.com/shazow/virtle/vm"
	"github.com/shazow/virtle/vm/vmtest"
)

const testTimeout = 5 * time.Second

type backendFunc func(context.Context, *vm.Spec) (backend.Machine, error)

func (f backendFunc) Start(ctx context.Context, spec *vm.Spec) (backend.Machine, error) {
	return f(ctx, spec)
}

// suspendingMachine models a backend with CLI-only suspend persistence: it
// queues control-socket suspend requests and records resume commits, the
// pieces the session bridge carries.
type suspendingMachine struct {
	backend.Machine
	suspendRequests chan struct{}
	handled         chan struct{}

	commitMu  sync.Mutex
	committed bool
}

func (m *suspendingMachine) SuspendRequests() <-chan struct{} { return m.suspendRequests }

func (m *suspendingMachine) HandleSuspendRequest(ctx context.Context) error {
	if err := m.Machine.(backend.Suspender).Suspend(ctx); err != nil {
		return err
	}
	close(m.handled)
	return sessionbridge.ErrSavedSuspendExit
}

func (m *suspendingMachine) CommitResume() error {
	m.commitMu.Lock()
	m.committed = true
	m.commitMu.Unlock()
	return nil
}

func (m *suspendingMachine) committedResume() bool {
	m.commitMu.Lock()
	defer m.commitMu.Unlock()
	return m.committed
}

type suspendingBackend struct {
	machine *suspendingMachine
	started chan struct{}
}

func (b *suspendingBackend) Start(ctx context.Context, spec *vm.Spec) (backend.Machine, error) {
	return b.start(ctx)
}

func (b *suspendingBackend) Resume(ctx context.Context, _ *vm.Spec) (backend.Machine, error) {
	return b.start(ctx)
}

func (*suspendingBackend) StateVersion() string { return "test-v1" }

func (b *suspendingBackend) start(ctx context.Context) (backend.Machine, error) {
	if bridge := sessionbridge.FromContext(ctx); bridge != nil {
		bridge.Bind(sessionbridge.Hooks{
			SuspendRequests:      b.machine.SuspendRequests,
			HandleSuspendRequest: b.machine.HandleSuspendRequest,
			Suspend:              b.machine.HandleSuspendRequest,
			CommitResume:         b.machine.CommitResume,
		})
	}
	close(b.started)
	return b.machine, nil
}

func newSuspendingBackend(t *testing.T) (*suspendingBackend, *suspendingMachine) {
	t.Helper()
	m := &suspendingMachine{
		Machine:         newMemoryMachine(t),
		suspendRequests: make(chan struct{}, 1),
		handled:         make(chan struct{}),
	}
	return &suspendingBackend{machine: m, started: make(chan struct{})}, m
}

// notifyingWriter closes wrote on its first write.
type notifyingWriter struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	wrote chan struct{}
	once  sync.Once
}

func (w *notifyingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	n, err := w.buf.Write(p)
	w.mu.Unlock()
	w.once.Do(func() { close(w.wrote) })
	return n, err
}

func (w *notifyingWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

type blockingBackend struct{ started chan struct{} }

func (b *blockingBackend) Start(ctx context.Context, _ *vm.Spec) (backend.Machine, error) {
	close(b.started)
	<-ctx.Done()
	return nil, context.Cause(ctx)
}

// notifyingBackend starts in-memory machines and reports each one; unlike
// backendtest's own backend it is not a backend.Resumer.
type notifyingBackend struct {
	delegate backend.Backend
	started  chan backend.Machine
}

func (b *notifyingBackend) Start(ctx context.Context, spec *vm.Spec) (backend.Machine, error) {
	m, err := b.delegate.Start(ctx, spec)
	if err == nil {
		b.started <- m
	}
	return m, err
}

func newNotifyingBackend(guest *vmtest.Guest) *notifyingBackend {
	return &notifyingBackend{delegate: backendtest.NewMemoryBackend(guest), started: make(chan backend.Machine, 1)}
}

// unsuspendable hides the in-memory machine's Suspend method.
type unsuspendable struct{ backend.Machine }

func newMemoryMachine(t *testing.T) backend.Machine {
	t.Helper()
	m, err := backendtest.NewMemoryBackend(nil).Start(context.Background(), &vm.Spec{})
	if err != nil {
		t.Fatalf("start memory machine: %v", err)
	}
	return m
}

func signalSelf(t *testing.T, sig os.Signal) {
	t.Helper()
	process, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatalf("FindProcess: %v", err)
	}
	if err := process.Signal(sig); err != nil {
		t.Fatalf("signal %v: %v", sig, err)
	}
}

func TestRunZeroOptionsStartFresh(t *testing.T) {
	called := false
	want := errors.New("start result")
	b := backendFunc(func(context.Context, *vm.Spec) (backend.Machine, error) { called = true; return nil, want })
	if err := Run(t.Context(), b, &vm.Spec{}, &manifest.Manifest{}, Options{}); !errors.Is(err, want) || !called {
		t.Fatalf("called %v, err %v", called, err)
	}
}

func TestRunReportsStartup(t *testing.T) {
	g := &vmtest.Guest{}
	b := newNotifyingBackend(g)
	var logs bytes.Buffer
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, b, &vm.Spec{}, &manifest.Manifest{}, Options{Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	}()
	<-b.started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
	if !strings.Contains(logs.String(), "vm startup complete") {
		t.Fatalf("startup log: %s", logs.String())
	}
}

func TestRunShutsMachineDownWhenContextEnds(t *testing.T) {
	g := &vmtest.Guest{}
	b := newNotifyingBackend(g)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, b, &vm.Spec{}, &manifest.Manifest{}, Options{Resume: ResumeNo})
	}()
	<-b.started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
	if got := g.Shutdowns(); got != 1 {
		t.Fatalf("guest shutdowns = %d, want 1", got)
	}
}

type ownedMachine struct {
	backend.Machine
	startCtx context.Context
	done     chan struct{}
	onStatus func()
	t        *testing.T
}

func (m *ownedMachine) Done() <-chan struct{} { return m.done }
func (m *ownedMachine) Shutdown(context.Context) error {
	if err := m.startCtx.Err(); err != nil {
		m.t.Error("machine lifetime canceled before graceful shutdown:", err)
	}
	close(m.done)
	return nil
}
func (m *ownedMachine) Status(context.Context) (backend.Status, error) {
	m.onStatus()
	return backend.Status{State: backend.StateReady}, nil
}

// TestRunOwnsGracefulShutdown checks that a signal ending the session lets
// Shutdown run before the machine's own context is canceled.
func TestRunOwnsGracefulShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// The foreground loop queries Status for the SSH hint once startup has
	// completed and it owns the machine; end the session right there.
	m := &ownedMachine{done: make(chan struct{}), onStatus: cancel, t: t}
	b := backendFunc(func(startCtx context.Context, _ *vm.Spec) (backend.Machine, error) {
		m.startCtx = startCtx
		return m, nil
	})
	mf := &manifest.Manifest{SSH: manifest.SSH{Argv: []string{"ssh"}}}
	opts := Options{Hooks: Hooks{SSHCommandHint: func(*manifest.Manifest, int) (string, error) { return "", nil }}}
	if err := Run(ctx, b, &vm.Spec{}, mf, opts); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
	select {
	case <-m.done:
	default:
		t.Fatal("machine was not shut down")
	}
}

func TestRunRejectsUnknownResumeModeBeforeStart(t *testing.T) {
	b := newNotifyingBackend(nil)
	err := Run(context.Background(), b, &vm.Spec{}, &manifest.Manifest{}, Options{Resume: "sometimes"})
	if err == nil {
		t.Fatal("Run unexpectedly accepted an unknown resume mode")
	}
	select {
	case <-b.started:
		t.Fatal("Run started a machine before rejecting the resume mode")
	default:
	}
}

func TestRunValidatesSSHBeforeStart(t *testing.T) {
	attach := func(context.Context, *Session) error { return nil }
	for _, tc := range []struct {
		name        string
		opts        Options
		mf          *manifest.Manifest
		unsupported bool
	}{
		{"backend without ssh attach", Options{SSH: true}, &manifest.Manifest{SSH: manifest.SSH{Argv: []string{"ssh"}}}, true},
		{"manifest without ssh.exec", Options{SSH: true, Hooks: Hooks{RunSSH: attach}}, &manifest.Manifest{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newNotifyingBackend(nil)
			err := Run(context.Background(), b, &vm.Spec{}, tc.mf, tc.opts)
			if err == nil {
				t.Fatal("Run unexpectedly accepted --ssh")
			}
			// A missing capability is detectable; a manifest mistake is not.
			if errors.Is(err, errors.ErrUnsupported) != tc.unsupported {
				t.Fatalf("Run error = %v, want ErrUnsupported %v", err, tc.unsupported)
			}
			select {
			case <-b.started:
				t.Fatal("Run started a machine before validating --ssh")
			default:
			}
		})
	}
}

func TestRunRequiresResumerForForcedResume(t *testing.T) {
	b := newNotifyingBackend(nil)
	err := Run(context.Background(), b, &vm.Spec{}, &manifest.Manifest{}, Options{Resume: ResumeForce})
	if !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("Run error = %v, want ErrUnsupported", err)
	}
	select {
	case <-b.started:
		t.Fatal("Run started a machine it could not resume")
	default:
	}
}

func TestRunInstallsInterruptHandlerBeforeStart(t *testing.T) {
	b := &blockingBackend{started: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), b, &vm.Spec{}, &manifest.Manifest{}, Options{Resume: ResumeNo})
	}()
	<-b.started
	signalSelf(t, os.Interrupt)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
}

func TestRunServicesQueuedSuspend(t *testing.T) {
	b, m := newSuspendingBackend(t)
	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), b, &vm.Spec{}, &manifest.Manifest{}, Options{Resume: ResumeNo})
	}()
	<-b.started
	m.suspendRequests <- struct{}{}
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	select {
	case <-m.handled:
	default:
		t.Fatal("queued suspend was not handled")
	}
}

func TestRunServicesSuspendWhileWaitingForReady(t *testing.T) {
	b, m := newSuspendingBackend(t)
	probeEnded := make(chan struct{})
	opts := Options{Resume: ResumeNo, Hooks: Hooks{Ready: func(ctx context.Context, _ backend.Machine) error {
		defer close(probeEnded)
		<-ctx.Done()
		return ctx.Err()
	}}}
	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), b, &vm.Spec{}, &manifest.Manifest{}, opts)
	}()
	<-b.started
	m.suspendRequests <- struct{}{}
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	select {
	case <-m.handled:
	default:
		t.Fatal("suspend requested during readiness was not handled")
	}
	select {
	case <-probeEnded:
	case <-time.After(testTimeout):
		t.Fatal("readiness probe was not canceled when the session ended")
	}
}

func TestRunShutsMachineDownWhenReadyFails(t *testing.T) {
	g := &vmtest.Guest{}
	b := newNotifyingBackend(g)
	notReady := errors.New("guest never came up")
	opts := Options{Resume: ResumeNo, Hooks: Hooks{Ready: func(context.Context, backend.Machine) error { return notReady }}}
	if err := Run(context.Background(), b, &vm.Spec{}, &manifest.Manifest{}, opts); !errors.Is(err, notReady) {
		t.Fatalf("Run error = %v, want %v", err, notReady)
	}
	if got := g.Shutdowns(); got != 1 {
		t.Fatalf("guest shutdowns = %d, want 1", got)
	}
}

func TestRunIgnoresSIGTSTPWhenMachineCannotSuspend(t *testing.T) {
	g := &vmtest.Guest{}
	started := make(chan struct{})
	b := backendFunc(func(ctx context.Context, spec *vm.Spec) (backend.Machine, error) {
		m, err := backendtest.NewMemoryBackend(g).Start(ctx, spec)
		close(started)
		return unsuspendable{m}, err
	})
	warnings := &notifyingWriter{wrote: make(chan struct{})}
	logger := slog.New(slog.NewTextHandler(warnings, &slog.HandlerOptions{Level: slog.LevelWarn}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, b, &vm.Spec{}, &manifest.Manifest{}, Options{Resume: ResumeNo, Logger: logger})
	}()
	<-started
	signalSelf(t, syscall.SIGTSTP)
	<-warnings.wrote
	select {
	case err := <-done:
		t.Fatalf("Run ended on SIGTSTP: %v", err)
	default:
	}
	if got := g.Shutdowns(); got != 0 {
		t.Fatalf("SIGTSTP shut the guest down %d times", got)
	}
	if !strings.Contains(warnings.String(), "cannot suspend") {
		t.Fatalf("warning = %q, want it to say the machine cannot suspend", warnings.String())
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
}

// TestRunTreatsSuspendDuringStartupAsCleanExit: a suspend request serviced
// while the backend was still starting saved the state and stopped the
// machine, so the launch exits cleanly, as it does after handoff.
func TestRunTreatsSuspendDuringStartupAsCleanExit(t *testing.T) {
	start := func(context.Context, backend.Backend, *vm.Spec, *manifest.Manifest, ResumeMode) (backend.Machine, bool, error) {
		return nil, false, sessionbridge.ErrSavedSuspendExit
	}
	err := Run(context.Background(), backendtest.NewMemoryBackend(nil), &vm.Spec{}, &manifest.Manifest{}, Options{Resume: ResumeNo, Hooks: Hooks{Start: start}})
	if err != nil {
		t.Fatalf("Run = %v, want nil after a suspend during startup", err)
	}
}

func TestRunPrintsSSHHintToStdout(t *testing.T) {
	b := newNotifyingBackend(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stdout := &notifyingWriter{wrote: make(chan struct{})}
	hint := func(mf *manifest.Manifest, cid int) (string, error) {
		return fmt.Sprintf("ssh %s@vsock/%d", mf.SSH.User, cid), nil
	}
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, b, &vm.Spec{}, &manifest.Manifest{SSH: manifest.SSH{Argv: []string{"ssh"}, User: "agent"}}, Options{
			Resume: ResumeNo, Stdout: stdout, Hooks: Hooks{SSHCommandHint: hint},
		})
	}()
	m := <-b.started
	status, err := m.(backend.StatusReporter).Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	<-stdout.wrote
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
	want := fmt.Sprintf("connect with ssh: ssh agent@vsock/%d\n", status.CID)
	if got := stdout.String(); got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestRunCommitsResumeOnlyOnceSSHIsEstablished(t *testing.T) {
	b, m := newSuspendingBackend(t)
	established := errors.New("ssh ended")
	attach := func(ctx context.Context, s *Session) error {
		if m.committedResume() {
			t.Error("resume state committed before the SSH session was established")
		}
		if err := s.Established(); err != nil {
			return err
		}
		return established
	}
	mf := &manifest.Manifest{SSH: manifest.SSH{Argv: []string{"ssh"}}}
	err := Run(context.Background(), b, &vm.Spec{}, mf, Options{Resume: ResumeForce, SSH: true, Hooks: Hooks{RunSSH: attach}})
	if !errors.Is(err, established) {
		t.Fatalf("Run error = %v, want %v", err, established)
	}
	if !m.committedResume() {
		t.Fatal("Established did not commit the restored state")
	}
}

func TestExitCode(t *testing.T) {
	exit3 := exec.Command("sh", "-c", "exit 3").Run()
	var exitErr *exec.ExitError
	if !errors.As(exit3, &exitErr) {
		t.Fatalf("sh -c 'exit 3' returned %v, want *exec.ExitError", exit3)
	}
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"success", nil, 0},
		{"foreground process status", fmt.Errorf("ssh: %w", exit3), 3},
		{"cancellation", fmt.Errorf("session: %w", context.Canceled), 130},
		{"other failure", errors.New("boom"), 1},
	} {
		if got := ExitCode(tc.err); got != tc.want {
			t.Errorf("%s: ExitCode(%v) = %d, want %d", tc.name, tc.err, got, tc.want)
		}
	}
}

// stallingMachine models a guest that never answers a shutdown request: its
// Shutdown returns only when the teardown context ends.
type stallingMachine struct {
	backend.Machine
	done         chan struct{}
	shuttingDown chan struct{}
	shutdownErr  error
}

func (m *stallingMachine) Done() <-chan struct{} { return m.done }
func (m *stallingMachine) Err() error            { return nil }
func (m *stallingMachine) Shutdown(ctx context.Context) error {
	close(m.shuttingDown)
	<-ctx.Done()
	m.shutdownErr = ctx.Err()
	close(m.done)
	return ctx.Err()
}

// TestRunKillsMachineOnSecondSignal checks that a second interrupt ends a
// graceful shutdown the guest is not answering, instead of being swallowed.
func TestRunKillsMachineOnSecondSignal(t *testing.T) {
	m := &stallingMachine{done: make(chan struct{}), shuttingDown: make(chan struct{})}
	started := make(chan struct{})
	b := backendFunc(func(context.Context, *vm.Spec) (backend.Machine, error) {
		close(started)
		return m, nil
	})
	var logs bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), b, &vm.Spec{}, &manifest.Manifest{}, Options{Resume: ResumeNo, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	}()
	<-started
	signalSelf(t, os.Interrupt)
	select {
	case <-m.shuttingDown:
	case <-time.After(testTimeout):
		t.Fatal("the first interrupt did not start the shutdown")
	}
	signalSelf(t, os.Interrupt)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v, want context.Canceled", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("the second interrupt did not end the shutdown")
	}
	if m.shutdownErr == nil {
		t.Fatal("the teardown context was not canceled")
	}
	if !strings.Contains(logs.String(), "second signal") {
		t.Fatalf("logs: %s", logs.String())
	}
}
