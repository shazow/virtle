// Package session owns the backend-neutral CLI foreground lifecycle: it
// starts (or resumes) one machine, waits for it to become ready, hands the
// terminal to an SSH session when asked, and turns signals and control-socket
// requests into an orderly suspend or shutdown. Backend-specific behavior
// enters only through Hooks.
package session

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/internal/executor"
	"github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/internal/sessionbridge"
	"github.com/shazow/virtle/vm"
)

// ResumeMode selects how saved suspend state is treated when a session starts.
type ResumeMode string

const (
	// ResumeAuto resumes when saved state exists and boots fresh otherwise.
	// The zero ResumeMode means ResumeAuto.
	ResumeAuto ResumeMode = "auto"
	// ResumeNo always boots fresh.
	ResumeNo ResumeMode = "no"
	// ResumeForce requires saved state and fails without it.
	ResumeForce ResumeMode = "force"
)

// Options configures a CLI session.
type Options struct {
	// Resume selects how saved suspend state is treated; the zero value is
	// ResumeAuto.
	Resume ResumeMode
	// SSH runs the manifest's ssh.exec command in the foreground once the
	// machine is ready, instead of waiting for the machine to exit.
	SSH bool
	// RemoteCommand is appended to the SSH command; it requires SSH.
	RemoteCommand []string
	// Logger receives session and SSH lifecycle logs; nil discards them.
	Logger *slog.Logger
	// Stdin, Stdout, and Stderr back the SSH session and the connection
	// hint. Nil selects the process standard streams.
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	// Hooks adapt a backend's CLI-only behavior to the shared loop. The zero
	// value runs the backend-neutral session: Start or Resume by capability,
	// ready as soon as Start returns, no SSH attach.
	Hooks Hooks
}

// Hooks are the backend-specific pieces of the foreground loop. The QEMU
// adapter in backend/qemu/session supplies them and the CLI wires them in;
// backends themselves know nothing about the session.
type Hooks struct {
	// Start starts or resumes the machine for mode and reports whether saved
	// state was resumed. Nil boots fresh for ResumeAuto and ResumeNo and
	// requires backend.Resumer for ResumeForce.
	Start func(ctx context.Context, b backend.Backend, spec *vm.Spec, mf *manifest.Manifest, mode ResumeMode) (backend.Machine, bool, error)
	// Ready blocks until a freshly started machine can take the foreground
	// session (for QEMU, until the guest signals SSH readiness). It must
	// return promptly once ctx ends. Nil treats the machine as ready when
	// Start returns.
	Ready func(ctx context.Context, m backend.Machine) error
	// RunSSH attaches the foreground SSH session and returns when it ends.
	// Nil means --ssh is unsupported for this backend.
	RunSSH func(ctx context.Context, s *Session) error
	// SSHCommandHint renders the connection hint printed after launch from
	// the manifest and the machine's vsock CID. Nil prints none.
	SSHCommandHint func(*manifest.Manifest, int) (string, error)
}

// Session is one foreground session: the running machine plus the shared
// loop's signal and suspend handling, handed to Hooks.RunSSH for the phases
// an SSH attach owns.
type Session struct {
	Machine  backend.Machine
	Manifest *manifest.Manifest
	Options  Options
	// Logger is Options.Logger, never nil.
	Logger *slog.Logger

	bridge  *sessionbridge.Bridge
	signals <-chan os.Signal
	logger  *slog.Logger // Logger scoped to this package
}

func start(ctx context.Context, b backend.Backend, spec *vm.Spec, _ *manifest.Manifest, mode ResumeMode) (backend.Machine, bool, error) {
	if mode == ResumeForce {
		resumer, ok := b.(backend.Resumer)
		if !ok {
			return nil, false, fmt.Errorf("backend cannot resume machines: %w", errors.ErrUnsupported)
		}
		m, err := resumer.Resume(ctx, spec)
		return m, true, err
	}
	m, err := b.Start(ctx, spec)
	return m, false, err
}

// Run starts a machine and owns it until exit, suspend, or signal shutdown.
func Run(ctx context.Context, b backend.Backend, spec *vm.Spec, mf *manifest.Manifest, opts Options) (err error) {
	if opts.Resume == "" {
		opts.Resume = ResumeAuto
	}
	if opts.Hooks.Start == nil {
		opts.Hooks.Start = start
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	logger := opts.Logger.With("package", "session")
	logger.Info("starting vm session", "resume", opts.Resume, "ssh", opts.SSH)
	defer func() {
		if err != nil {
			logger.Info("vm session ended", "err", err)
			return
		}
		logger.Info("vm session ended")
	}()
	switch opts.Resume {
	case ResumeAuto, ResumeNo, ResumeForce:
	default:
		return fmt.Errorf("invalid resume mode %q", opts.Resume)
	}
	if opts.SSH {
		if opts.Hooks.RunSSH == nil {
			return fmt.Errorf("--ssh: this backend cannot attach SSH sessions: %w", errors.ErrUnsupported)
		}
		if len(mf.SSH.Argv) == 0 {
			return fmt.Errorf("--ssh requires a non-empty manifest.ssh.exec")
		}
	}

	runCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGTSTP, syscall.SIGUSR1)
	defer signal.Stop(signals)
	bridge := &sessionbridge.Bridge{}
	runCtx = sessionbridge.WithContext(runCtx, bridge)

	// Startup is cancelable, but after handoff this foreground session owns
	// graceful teardown. Do not let a signal independently kill the backend
	// before Shutdown has had a chance to ask the guest to stop.
	machineCtx, cancelMachine := context.WithCancel(context.WithoutCancel(runCtx))
	defer cancelMachine()
	stopStartupCancel := context.AfterFunc(runCtx, cancelMachine)
	m, resumed, err := opts.Hooks.Start(machineCtx, b, spec, mf, opts.Resume)
	stopStartupCancel()
	if err != nil {
		if sessionbridge.IsSavedSuspendExit(err) {
			// A suspend request serviced during startup saved the state and
			// stopped the machine: a clean exit, as it is after handoff.
			return nil
		}
		return err
	}
	s := &Session{Machine: m, Manifest: mf, Options: opts, Logger: opts.Logger, bridge: bridge, signals: signals, logger: logger}
	if !resumed {
		if err := s.waitReady(runCtx); err != nil {
			if sessionbridge.IsSavedSuspendExit(err) {
				return nil
			}
			return shutdownAfter(runCtx, m, err)
		}
		logger.Info("vm startup complete")
	}
	logger.Info("vm started; entering foreground session")
	err = s.foreground(runCtx)
	if sessionbridge.IsSavedSuspendExit(err) {
		return nil
	}
	return err
}

func shutdownAfter(ctx context.Context, m backend.Machine, err error) error {
	return errors.Join(err, m.Shutdown(context.WithoutCancel(ctx)))
}

// afterSuspend reports a suspend outcome. A saved-state exit needs no
// teardown (the machine is already down); any other failure shuts m down.
func afterSuspend(ctx context.Context, m backend.Machine, err error) error {
	if err == nil || sessionbridge.IsSavedSuspendExit(err) {
		return err
	}
	return shutdownAfter(ctx, m, err)
}

// waitReady runs Hooks.Ready while still servicing machine exit, suspend
// requests, and signals, none of which the probe itself knows about.
func (s *Session) waitReady(ctx context.Context) error {
	ready := s.Options.Hooks.Ready
	if ready == nil {
		return nil
	}
	s.logger.Info("waiting for machine readiness")
	probeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- ready(probeCtx, s.Machine) }()
	for {
		select {
		case err := <-result:
			return err
		case <-s.Machine.Done():
			if err := s.Machine.Err(); err != nil {
				return err
			}
			return errors.New("machine exited before it was ready")
		case <-s.bridge.Requests():
			return s.bridge.HandleSuspend(ctx)
		case sig := <-s.signals:
			if s.wantsSuspend(ctx, sig) {
				return s.suspend(ctx)
			}
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
}

func (s *Session) foreground(ctx context.Context) error {
	m := s.Machine
	if s.Options.SSH {
		err := s.Options.Hooks.RunSSH(ctx, s)
		if sessionbridge.IsSavedSuspendExit(err) {
			return err
		}
		return shutdownAfter(ctx, m, err)
	}

	if reporter, ok := m.(backend.StatusReporter); ok && len(s.Manifest.SSH.Argv) > 0 && s.Options.Hooks.SSHCommandHint != nil {
		status, err := reporter.Status(ctx)
		if err != nil {
			return shutdownAfter(ctx, m, err)
		}
		hint, err := s.Options.Hooks.SSHCommandHint(s.Manifest, status.CID)
		if err != nil {
			s.logger.Warn("ssh command hint template failed", "err", err)
		} else if hint != "" {
			if _, err := fmt.Fprintf(cmp.Or[io.Writer](s.Options.Stdout, os.Stdout), "connect with ssh: %s\n", hint); err != nil {
				return shutdownAfter(ctx, m, fmt.Errorf("write ssh command hint: %w", err))
			}
		}
	}
	if err := s.Established(); err != nil {
		return shutdownAfter(ctx, m, err)
	}
	return s.waitForMachine(ctx)
}

func (s *Session) waitForMachine(ctx context.Context) error {
	m := s.Machine
	for {
		select {
		case <-m.Done():
			return m.Err()
		case <-s.bridge.Requests():
			return afterSuspend(ctx, m, s.bridge.HandleSuspend(ctx))
		case sig := <-s.signals:
			if s.wantsSuspend(ctx, sig) {
				return afterSuspend(ctx, m, s.suspend(ctx))
			}
		case <-ctx.Done():
			return shutdownAfter(ctx, m, context.Cause(ctx))
		}
	}
}

// WaitProcess waits for the foreground SSH process while servicing machine
// exit, suspend requests, and signals; whichever of those ends the wait first
// stops the process. When the process ends on its own its exit result is
// returned.
func (s *Session) WaitProcess(ctx context.Context, process *executor.Process) error {
	m := s.Machine
	stopProcess := func() { _ = process.Stop(context.Background()) }
	for {
		select {
		case <-process.Done():
			return process.Wait()
		case <-m.Done():
			stopProcess()
			return m.Err()
		case <-s.bridge.Requests():
			stopProcess()
			return s.bridge.HandleSuspend(ctx)
		case sig := <-s.signals:
			if s.wantsSuspend(ctx, sig) {
				stopProcess()
				return s.suspend(ctx)
			}
		case <-ctx.Done():
			stopProcess()
			return context.Cause(ctx)
		}
	}
}

// WaitRetry waits delay before the next SSH connection attempt, ending early
// when the machine exits, a suspend is requested, or ctx ends.
func (s *Session) WaitRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	m := s.Machine
	for {
		select {
		case <-timer.C:
			return nil
		case <-m.Done():
			if err := m.Err(); err != nil {
				return err
			}
			return errors.New("machine exited before SSH retry")
		case <-s.bridge.Requests():
			return s.bridge.HandleSuspend(ctx)
		case sig := <-s.signals:
			if s.wantsSuspend(ctx, sig) {
				return s.suspend(ctx)
			}
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
}

// Established marks the foreground session as established: state restored by
// a resume is discarded now that the session is using it.
func (s *Session) Established() error { return s.bridge.Commit() }

// wantsSuspend services one foreground signal and reports whether it asks
// for a suspend. SIGUSR1 logs the machine status. SIGTSTP suspends when the
// machine can suspend and is otherwise ignored with a warning, so a stray ^Z
// does not tear the machine down.
func (s *Session) wantsSuspend(ctx context.Context, sig os.Signal) bool {
	switch sig {
	case syscall.SIGTSTP:
		if s.canSuspend() {
			return true
		}
		s.logger.Warn("ignoring SIGTSTP: this machine cannot suspend")
	case syscall.SIGUSR1:
		s.logStatus(ctx)
	}
	return false
}

func (s *Session) canSuspend() bool {
	if s.bridge.CanSuspend() {
		return true
	}
	_, ok := s.Machine.(backend.Suspender)
	return ok
}

func (s *Session) suspend(ctx context.Context) error {
	if s.bridge.CanSuspend() {
		return s.bridge.Suspend(ctx)
	}
	suspender, ok := s.Machine.(backend.Suspender)
	if !ok {
		return fmt.Errorf("machine cannot suspend: %w", errors.ErrUnsupported)
	}
	return suspender.Suspend(ctx)
}

func (s *Session) logStatus(ctx context.Context) {
	if reporter, ok := s.Machine.(backend.StatusReporter); ok {
		if status, err := reporter.Status(ctx); err == nil {
			s.logger.Info("machine status", "state", status.State, "cid", status.CID, "pid", status.PID)
		}
	}
}

// ExitCode maps session errors onto CLI exit codes: a foreground process
// (SSH, a helper) that exited with a status passes that status through,
// cancellation is 130, and anything else is 1.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		// ExitCode is -1 for a signal-killed child; fall through to the
		// generic failure code rather than exiting 255.
		if code := exitErr.ExitCode(); code >= 0 {
			return code
		}
	}
	if errors.Is(err, context.Canceled) {
		return 130
	}
	return 1
}
