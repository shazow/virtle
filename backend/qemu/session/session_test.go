package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/backend/backendtest"
	"github.com/shazow/virtle/backend/qemu/internal/launch"
	"github.com/shazow/virtle/backend/qemu/internal/sessionbridge"
	"github.com/shazow/virtle/internal/executor"
	"github.com/shazow/virtle/internal/executor/executortest"
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
	return b.start(ctx)
}

func (b *sessionTestBackend) Resume(ctx context.Context, _ *vm.Spec) (backend.Machine, error) {
	return b.start(ctx)
}

func (*sessionTestBackend) StateVersion() string { return "test-v1" }

func (b *sessionTestBackend) start(ctx context.Context) (backend.Machine, error) {
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

type sessionRunner struct {
	mu       sync.Mutex
	waitErrs []error
	commands []*exec.Cmd
}

type notifyingWriter struct {
	bytes.Buffer
	wrote chan struct{}
	once  sync.Once
}

func (w *notifyingWriter) Write(p []byte) (int, error) {
	n, err := w.Buffer.Write(p)
	w.once.Do(func() { close(w.wrote) })
	return n, err
}

func (r *sessionRunner) Start(cmd *exec.Cmd) (*executor.Process, error) {
	r.mu.Lock()
	r.commands = append(r.commands, cmd)
	var waitErr error
	if len(r.waitErrs) > 0 {
		waitErr = r.waitErrs[0]
		r.waitErrs = r.waitErrs[1:]
	}
	r.mu.Unlock()
	return (&executortest.Process{Exited: true, WaitErr: waitErr}).Process(), nil
}

func (r *sessionRunner) starts() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.commands)
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

func TestRunRetriesTransientSSHFailure(t *testing.T) {
	g := &vmtest.Guest{}
	b := newNotifyingBackend(g)
	runner := &sessionRunner{waitErrs: []error{errors.New("Connection refused"), nil}}
	mf := &manifest.Manifest{
		Paths: manifest.Paths{WorkingDir: t.TempDir()},
		SSH:   manifest.SSH{Argv: []string{"ssh"}, User: "agent", RetryDelay: time.Millisecond},
	}
	err := Run(context.Background(), b, &vm.Spec{}, mf, Options{Resume: "no", SSH: true, runner: runner, Stdin: bytes.NewReader(nil), Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, want := runner.starts(), 2; got != want {
		t.Fatalf("SSH starts = %d, want %d", got, want)
	}
}

func TestInstallSSHKeyWritesTemporaryAuthorizedKey(t *testing.T) {
	guest := &vmtest.Guest{Commands: map[string]vmtest.Result{
		"/bin/sh": {},
		"chown":   {},
		"chmod":   {},
	}}
	m, err := backendtest.NewMemoryBackend(guest).Start(context.Background(), &vm.Spec{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	want := "ssh-ed25519 AAAA-test virtle-test\n"
	if err := installSSHKey(context.Background(), m, &manifest.Manifest{
		SSH: manifest.SSH{User: "agent"},
	}, launch.SSHAutoprovisionKey{AuthorizedKey: want[:len(want)-1]}); err != nil {
		t.Fatalf("installSSHKey: %v", err)
	}
	file := guest.FS["/run/virtle-autoprovision-authorized-key.pub"]
	if file == nil || string(file.Data) != want || file.Mode != 0o600 {
		t.Fatalf("temporary key = %#v, want data %q and mode 0600", file, want)
	}
}

func TestRunPrintsSSHHintToStdout(t *testing.T) {
	b := newNotifyingBackend(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stdout := &notifyingWriter{wrote: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, b, &vm.Spec{}, &manifest.Manifest{SSH: manifest.SSH{Argv: []string{"ssh"}, User: "agent"}}, Options{
			Resume: "no", Stdout: stdout,
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

func TestWaitReadyHasDeadline(t *testing.T) {
	t.Setenv(sshReadyTimeoutEnv, "20ms")
	path := filepath.Join(t.TempDir(), "ready.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	m := &sessionTestMachine{Machine: newMemoryMachine(t), readyPath: path}
	err = waitReady(context.Background(), m, &sessionbridge.Bridge{}, make(chan os.Signal), slog.New(slog.DiscardHandler))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waitReady error = %v, want context.DeadlineExceeded", err)
	}
}
