package session

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/backend/backendtest"
	"github.com/shazow/virtle/backend/qemu/internal/launch"
	"github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/vm"
	"github.com/shazow/virtle/vm/vmtest"
)

type sessionTestMachine struct {
	backend.Machine
	suspendRequests chan struct{}
	handled         chan struct{}

	commitMu  sync.Mutex
	committed bool
	readyPath string
}

func (m *sessionTestMachine) SuspendRequests() <-chan struct{} { return m.suspendRequests }

func (m *sessionTestMachine) HandleSuspendRequest(ctx context.Context) error {
	if err := m.Machine.(backend.Suspender).Suspend(ctx); err != nil {
		return err
	}
	close(m.handled)
	return launch.ErrSavedSuspendExit
}

func (m *sessionTestMachine) CommitResume() error {
	m.commitMu.Lock()
	m.committed = true
	m.commitMu.Unlock()
	return nil
}

func (m *sessionTestMachine) committedResume() bool {
	m.commitMu.Lock()
	defer m.commitMu.Unlock()
	return m.committed
}

func (m *sessionTestMachine) Status(ctx context.Context) (backend.Status, error) {
	status, err := m.Machine.(backend.StatusReporter).Status(ctx)
	status.Paths.ReadySocket = m.readyPath
	return status, err
}

type sessionTestBackend struct {
	machine *sessionTestMachine
	started chan struct{}
}

func (b *sessionTestBackend) Start(ctx context.Context, spec *vm.Spec) (backend.Machine, error) {
	return b.StartSession(ctx, spec, false)
}

func (b *sessionTestBackend) StartSession(context.Context, *vm.Spec, bool) (backend.Machine, error) {
	close(b.started)
	return b.machine, nil
}

type blockingBackend struct{ started chan struct{} }

func (b *blockingBackend) Start(ctx context.Context, _ *vm.Spec) (backend.Machine, error) {
	close(b.started)
	<-ctx.Done()
	return nil, context.Cause(ctx)
}

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

func newMemoryMachine(t *testing.T) backend.Machine {
	t.Helper()
	m, err := backendtest.NewMemoryBackend(nil).Start(context.Background(), &vm.Spec{})
	if err != nil {
		t.Fatalf("start memory machine: %v", err)
	}
	return m
}

func TestRunShutsMachineDownWhenContextEnds(t *testing.T) {
	g := &vmtest.Guest{}
	b := newNotifyingBackend(g)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, b, &vm.Spec{}, &manifest.Manifest{}, Options{Resume: "no"})
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
	b := newNotifyingBackend(nil)
	err := Run(context.Background(), b, &vm.Spec{}, &manifest.Manifest{}, Options{Resume: "no", SSH: true})
	if err == nil {
		t.Fatal("Run unexpectedly accepted an empty ssh.exec")
	}
	select {
	case <-b.started:
		t.Fatal("Run started a machine before validating ssh.exec")
	default:
	}
}

func TestRunInstallsInterruptHandlerBeforeStart(t *testing.T) {
	b := &blockingBackend{started: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), b, &vm.Spec{}, &manifest.Manifest{}, Options{Resume: "no"})
	}()
	<-b.started
	process, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatalf("FindProcess: %v", err)
	}
	if err := process.Signal(os.Interrupt); err != nil {
		t.Fatalf("signal interrupt: %v", err)
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
}

func TestRunServicesQueuedSuspend(t *testing.T) {
	m := &sessionTestMachine{
		Machine:         newMemoryMachine(t),
		suspendRequests: make(chan struct{}, 1),
		handled:         make(chan struct{}),
	}
	b := &sessionTestBackend{machine: m, started: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), b, &vm.Spec{}, &manifest.Manifest{}, Options{Resume: "no"})
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

func TestRunPreservesResumeStateWhenSSHStartFails(t *testing.T) {
	m := &sessionTestMachine{
		Machine:         newMemoryMachine(t),
		suspendRequests: make(chan struct{}, 1),
		handled:         make(chan struct{}),
	}
	b := &sessionTestBackend{machine: m, started: make(chan struct{})}
	mf := &manifest.Manifest{SSH: manifest.SSH{Argv: []string{filepath.Join(t.TempDir(), "missing-ssh")}}}
	err := Run(context.Background(), b, &vm.Spec{}, mf, Options{Resume: "force", SSH: true})
	if err == nil {
		t.Fatal("Run unexpectedly started a missing SSH executable")
	}
	if m.committedResume() {
		t.Fatal("Run committed restored state before the SSH process started")
	}
}

func TestWaitReadyHasDeadline(t *testing.T) {
	t.Setenv(sshReadyTimeoutEnv, "20ms")
	path := filepath.Join(t.TempDir(), "ready.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	m := &sessionTestMachine{Machine: newMemoryMachine(t), readyPath: path}
	err = waitReady(context.Background(), m, make(chan os.Signal), slog.New(slog.DiscardHandler))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waitReady error = %v, want context.DeadlineExceeded", err)
	}
}
