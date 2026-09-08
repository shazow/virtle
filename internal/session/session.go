// Package session owns the backend-neutral CLI foreground lifecycle.
package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/internal/executor"
	"github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/internal/readiness"
	"github.com/shazow/virtle/internal/sessionbridge"
	"github.com/shazow/virtle/vm"
)

const (
	defaultSSHReadyTimeout = 2 * time.Minute
	sshReadyTimeoutEnv     = "VIRTLE_SSH_READY_TIMEOUT"
	sshReadyToken          = "SSH-READY"
)

type Options struct {
	hooks Hooks
	// Resume selects how saved suspend state is treated: "auto" (the default
	// when empty) resumes when a save exists, "force" requires one, and "no"
	// always boots fresh.
	Resume string
	// SSH runs the manifest's ssh.exec command in the foreground once the
	// guest reports readiness, instead of waiting for the machine to exit.
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
}

// Hooks supplies backend-specific suspend persistence and SSH behavior.
// A backend may implement SessionHooks() Hooks to provide these to Run.
type Hooks struct {
	Start          func(context.Context, backend.Backend, *vm.Spec, *manifest.Manifest, string) (backend.Machine, bool, error)
	RunSSH         func(context.Context, backend.Machine, *sessionbridge.Bridge, *manifest.Manifest, Options, <-chan os.Signal, *slog.Logger, *slog.Logger) error
	SSHCommandHint func(*manifest.Manifest, int) (string, error)
}

// Run starts the selected backend and owns its foreground lifecycle.
func Run(ctx context.Context, b backend.Backend, spec *vm.Spec, mf *manifest.Manifest, opts Options) error {
	var hooks Hooks
	if provider, ok := b.(interface{ SessionHooks() Hooks }); ok {
		hooks = provider.SessionHooks()
	}
	return RunWithHooks(ctx, b, spec, mf, opts, hooks)
}

func start(ctx context.Context, b backend.Backend, spec *vm.Spec, _ *manifest.Manifest, mode string) (backend.Machine, bool, error) {
	if mode == "force" {
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

// RunWithHooks runs a session with explicit backend adapters.
func RunWithHooks(ctx context.Context, b backend.Backend, spec *vm.Spec, mf *manifest.Manifest, opts Options, hooks Hooks) (err error) {
	opts.hooks = hooks
	if opts.hooks.Start == nil {
		opts.hooks.Start = start
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	sessionLogger := logger.With("package", "session")
	sshLogger := logger.With("package", "ssh")
	sessionLogger.Info("starting vm session", "resume", opts.Resume, "ssh", opts.SSH)
	defer func() {
		if err != nil {
			sessionLogger.Info("vm session ended", "err", err)
			return
		}
		sessionLogger.Info("vm session ended")
	}()
	if opts.SSH && len(mf.SSH.Argv) == 0 {
		return fmt.Errorf("--ssh requires a non-empty manifest.ssh.exec")
	}
	switch opts.Resume {
	case "", "auto", "no", "force":
	default:
		return fmt.Errorf("invalid resume mode %q", opts.Resume)
	}
	if opts.SSH && opts.hooks.RunSSH == nil {
		return fmt.Errorf("backend cannot attach SSH: %w", errors.ErrUnsupported)
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
	m, resumed, err := opts.hooks.Start(machineCtx, b, spec, mf, opts.Resume)
	stopStartupCancel()
	if err != nil {
		return err
	}
	if !resumed {
		if err := WaitReady(runCtx, m, bridge, signals, sshLogger); err != nil {
			if sessionbridge.IsSavedSuspendExit(err) {
				return nil
			}
			return shutdownAfter(runCtx, m, err)
		}
		sessionLogger.Info("vm startup complete")
	}
	sessionLogger.Info("vm started; entering foreground session")
	err = foreground(runCtx, m, bridge, mf, opts, signals, sessionLogger, sshLogger)
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

func WaitReady(ctx context.Context, m backend.Machine, bridge *sessionbridge.Bridge, signals <-chan os.Signal, logger *slog.Logger) error {
	reporter, ok := m.(backend.StatusReporter)
	if !ok {
		return nil
	}
	status, err := reporter.Status(ctx)
	if err != nil {
		return err
	}
	if status.Paths.ReadySocket == "" {
		return nil
	}
	logger.Info("waiting for ssh readiness")
	readyCtx, cancel := context.WithTimeout(ctx, readiness.TimeoutFromEnv(sshReadyTimeoutEnv, defaultSSHReadyTimeout))
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(readyCtx, "unix", status.Paths.ReadySocket)
	if err != nil {
		return fmt.Errorf("connect readiness socket: %w", err)
	}
	defer conn.Close()
	errCh := make(chan error, 1)
	go func() { errCh <- readiness.ReadToken(conn, sshReadyToken) }()
	for {
		select {
		case err := <-errCh:
			return err
		case <-m.Done():
			if err := m.Err(); err != nil {
				return err
			}
			return errors.New("machine exited before SSH readiness")
		case <-bridge.Requests():
			return bridge.HandleSuspend(readyCtx)
		case sig := <-signals:
			if sig == syscall.SIGTSTP {
				return suspend(readyCtx, m, bridge)
			}
			logStatus(readyCtx, m, logger)
		case <-readyCtx.Done():
			return fmt.Errorf("wait for SSH readiness: %w", context.Cause(readyCtx))
		}
	}
}

func foreground(ctx context.Context, m backend.Machine, bridge *sessionbridge.Bridge, mf *manifest.Manifest, opts Options, signals <-chan os.Signal, logger *slog.Logger, sshLogger *slog.Logger) error {
	if opts.SSH {
		err := opts.hooks.RunSSH(ctx, m, bridge, mf, opts, signals, logger, sshLogger)
		if sessionbridge.IsSavedSuspendExit(err) {
			return err
		}
		return shutdownAfter(ctx, m, err)
	}

	if reporter, ok := m.(backend.StatusReporter); ok && len(mf.SSH.Argv) > 0 && opts.hooks.SSHCommandHint != nil {
		status, err := reporter.Status(ctx)
		if err != nil {
			return shutdownAfter(ctx, m, err)
		}
		hint, err := opts.hooks.SSHCommandHint(mf, status.CID)
		if err != nil {
			logger.Warn("ssh command hint template failed", "err", err)
		} else if hint != "" {
			if _, err := fmt.Fprintf(optionWriter(opts.Stdout, os.Stdout), "connect with ssh: %s\n", hint); err != nil {
				return shutdownAfter(ctx, m, fmt.Errorf("write ssh command hint: %w", err))
			}
		}
	}
	if err := bridge.Commit(); err != nil {
		return shutdownAfter(ctx, m, err)
	}
	return waitForMachine(ctx, m, bridge, signals, logger)
}

func waitForMachine(ctx context.Context, m backend.Machine, bridge *sessionbridge.Bridge, signals <-chan os.Signal, logger *slog.Logger) error {
	for {
		select {
		case <-m.Done():
			return m.Err()
		case <-bridge.Requests():
			return afterSuspend(ctx, m, bridge.HandleSuspend(ctx))
		case sig := <-signals:
			switch sig {
			case syscall.SIGUSR1:
				logStatus(ctx, m, logger)
			case syscall.SIGTSTP:
				return afterSuspend(ctx, m, suspend(ctx, m, bridge))
			}
		case <-ctx.Done():
			return shutdownAfter(ctx, m, context.Cause(ctx))
		}
	}
}

func WaitForSSHProcess(ctx context.Context, m backend.Machine, bridge *sessionbridge.Bridge, process *executor.Process, signals <-chan os.Signal, logger *slog.Logger) error {
	for {
		select {
		case <-process.Done():
			return process.Wait()
		case <-m.Done():
			_ = process.Stop(context.Background())
			return m.Err()
		case <-bridge.Requests():
			_ = process.Stop(context.Background())
			return bridge.HandleSuspend(ctx)
		case sig := <-signals:
			switch sig {
			case syscall.SIGUSR1:
				logStatus(ctx, m, logger)
			case syscall.SIGTSTP:
				_ = process.Stop(context.Background())
				return suspend(ctx, m, bridge)
			}
		case <-ctx.Done():
			_ = process.Stop(context.Background())
			return context.Cause(ctx)
		}
	}
}

func WaitForSSHRetry(ctx context.Context, m backend.Machine, bridge *sessionbridge.Bridge, delay time.Duration, signals <-chan os.Signal, logger *slog.Logger) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			return nil
		case <-m.Done():
			if err := m.Err(); err != nil {
				return err
			}
			return errors.New("machine exited before SSH retry")
		case <-bridge.Requests():
			return bridge.HandleSuspend(ctx)
		case sig := <-signals:
			switch sig {
			case syscall.SIGUSR1:
				logStatus(ctx, m, logger)
			case syscall.SIGTSTP:
				return suspend(ctx, m, bridge)
			}
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
}

func suspend(ctx context.Context, m backend.Machine, bridge *sessionbridge.Bridge) error {
	if bridge.Requests() != nil {
		return bridge.Suspend(ctx)
	}
	suspender, ok := m.(backend.Suspender)
	if !ok {
		return fmt.Errorf("machine cannot suspend: %w", errors.ErrUnsupported)
	}
	return suspender.Suspend(ctx)
}

func optionWriter(configured io.Writer, fallback io.Writer) io.Writer {
	if configured != nil {
		return configured
	}
	return fallback
}

func logStatus(ctx context.Context, m backend.Machine, logger *slog.Logger) {
	if reporter, ok := m.(backend.StatusReporter); ok {
		if status, err := reporter.Status(ctx); err == nil {
			logger.Info("machine status", "state", status.State, "cid", status.CID, "pid", status.PID)
		}
	}
}

// ExitCode maps session errors onto CLI exit codes.

func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var commandErr interface {
		error
		SessionExitCode() int
	}
	if errors.As(err, &commandErr) && commandErr.SessionExitCode() >= 0 {
		return commandErr.SessionExitCode()
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
