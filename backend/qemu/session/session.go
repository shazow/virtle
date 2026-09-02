// Package session implements the virtle CLI foreground loop over the public
// backend machine contract.
package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/backend/qemu/internal/launch"
	"github.com/shazow/virtle/internal/executor"
	"github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/internal/readiness"
	"github.com/shazow/virtle/internal/sshtools"
	"github.com/shazow/virtle/vm"
)

const (
	defaultSSHReadyTimeout = 2 * time.Minute
	sshReadyTimeoutEnv     = "VIRTLE_SSH_READY_TIMEOUT"
)

type sessionStarter interface {
	StartSession(context.Context, *vm.Spec, bool) (backend.Machine, error)
}

type resumeCommitter interface {
	CommitResume() error
}

type suspendRequestHandler interface {
	SuspendRequests() <-chan struct{}
	HandleSuspendRequest(context.Context) error
}

// Options configures a CLI session.
type Options struct {
	Resume        string
	SSH           bool
	RemoteCommand []string
	Logger        *slog.Logger
}

// Run starts a machine and owns it until exit, suspend, or signal shutdown.
func Run(ctx context.Context, b backend.Backend, spec *vm.Spec, mf *manifest.Manifest, opts Options) error {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if opts.SSH && len(mf.SSH.Argv) == 0 {
		return fmt.Errorf("--ssh requires a non-empty manifest.ssh.exec")
	}
	if err := validateResumeMode(opts.Resume); err != nil {
		return err
	}

	runCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGTSTP, syscall.SIGUSR1)
	defer signal.Stop(signals)

	m, resumed, err := start(runCtx, b, spec, mf, opts.Resume)
	if err != nil {
		return err
	}
	if !resumed {
		if err := waitReady(runCtx, m, signals, logger.With("package", "session")); err != nil {
			if launch.IsSavedSuspendExit(err) {
				return nil
			}
			return errors.Join(err, m.Shutdown(context.WithoutCancel(runCtx)))
		}
	}
	err = foreground(runCtx, m, mf, opts, signals, logger.With("package", "session"))
	if launch.IsSavedSuspendExit(err) {
		return nil
	}
	return err
}

func start(ctx context.Context, b backend.Backend, spec *vm.Spec, mf *manifest.Manifest, mode string) (backend.Machine, bool, error) {
	if mode == "" {
		mode = "auto"
	}
	resume := mode == "force"
	if mode == "auto" {
		saved, err := launch.HasSavedSuspendState(mf)
		if err != nil {
			return nil, false, err
		}
		resume = saved
	}
	if starter, ok := b.(sessionStarter); ok {
		m, err := starter.StartSession(ctx, spec, resume)
		return m, resume, err
	}
	if !resume {
		m, err := b.Start(ctx, spec)
		return m, false, err
	}
	r, ok := b.(backend.Resumer)
	if !ok {
		return nil, false, fmt.Errorf("backend cannot resume machines: %w", errors.ErrUnsupported)
	}
	m, err := r.Resume(ctx, spec)
	return m, true, err
}

func validateResumeMode(mode string) error {
	if mode == "" || mode == "no" || mode == "auto" || mode == "force" {
		return nil
	}
	return fmt.Errorf("unsupported resume mode %q", mode)
}

func waitReady(ctx context.Context, m backend.Machine, signals <-chan os.Signal, logger *slog.Logger) error {
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
	readyCtx, cancel := context.WithTimeout(ctx, readiness.TimeoutFromEnv(sshReadyTimeoutEnv, defaultSSHReadyTimeout))
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(readyCtx, "unix", status.Paths.ReadySocket)
	if err != nil {
		return fmt.Errorf("connect readiness socket: %w", err)
	}
	defer conn.Close()
	errCh := make(chan error, 1)
	go func() { errCh <- readiness.ReadToken(conn, launch.SSHReadyToken) }()
	for {
		select {
		case err := <-errCh:
			return err
		case <-m.Done():
			return m.Err()
		case <-suspendRequests(m):
			return handleSuspendRequest(readyCtx, m)
		case sig := <-signals:
			if sig == syscall.SIGTSTP {
				return suspend(readyCtx, m)
			}
			logStatus(readyCtx, m, logger)
		case <-readyCtx.Done():
			return fmt.Errorf("wait for SSH readiness: %w", context.Cause(readyCtx))
		}
	}
}

func foreground(ctx context.Context, m backend.Machine, mf *manifest.Manifest, opts Options, signals <-chan os.Signal, logger *slog.Logger) error {
	var session *executor.Process
	if opts.SSH {
		reporter, ok := m.(backend.StatusReporter)
		if !ok {
			return errors.Join(fmt.Errorf("machine cannot report its SSH destination: %w", errors.ErrUnsupported), m.Shutdown(context.WithoutCancel(ctx)))
		}
		status, err := reporter.Status(ctx)
		if err != nil {
			return errors.Join(err, m.Shutdown(context.WithoutCancel(ctx)))
		}
		cmd, err := sshCommand(mf, status.CID, opts.RemoteCommand)
		if err != nil {
			return errors.Join(err, m.Shutdown(context.WithoutCancel(ctx)))
		}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		session, err = (&executor.Runner{Logger: logger}).Start(cmd)
		if err != nil {
			return errors.Join(err, m.Shutdown(context.WithoutCancel(ctx)))
		}
	} else if reporter, ok := m.(backend.StatusReporter); ok && len(mf.SSH.Argv) > 0 {
		if status, err := reporter.Status(ctx); err == nil {
			if hint, err := launch.BuildSSHCommandHint(mf, status.CID); err == nil {
				logger.Info("connect to the guest", "command", hint)
			}
		}
	}
	if committer, ok := m.(resumeCommitter); ok {
		if err := committer.CommitResume(); err != nil {
			return errors.Join(err, m.Shutdown(context.WithoutCancel(ctx)))
		}
	}

	for {
		var sessionDone <-chan struct{}
		if session != nil {
			sessionDone = session.Done()
		}
		select {
		case <-m.Done():
			if session != nil {
				_ = session.Stop(context.Background())
			}
			return m.Err()
		case <-sessionDone:
			err := session.Wait()
			return errors.Join(err, m.Shutdown(context.WithoutCancel(ctx)))
		case <-suspendRequests(m):
			if session != nil {
				_ = session.Stop(context.Background())
			}
			if err := handleSuspendRequest(ctx, m); err != nil {
				return errors.Join(err, m.Shutdown(context.WithoutCancel(ctx)))
			}
			return nil
		case sig := <-signals:
			switch sig {
			case syscall.SIGUSR1:
				logStatus(ctx, m, logger)
			case syscall.SIGTSTP:
				if session != nil {
					_ = session.Stop(context.Background())
				}
				if err := suspend(ctx, m); err != nil {
					return errors.Join(err, m.Shutdown(context.WithoutCancel(ctx)))
				}
				return nil
			}
		case <-ctx.Done():
			if session != nil {
				_ = session.Stop(context.Background())
			}
			return errors.Join(context.Cause(ctx), m.Shutdown(context.WithoutCancel(ctx)))
		}
	}
}

func suspendRequests(m backend.Machine) <-chan struct{} {
	if handler, ok := m.(suspendRequestHandler); ok {
		return handler.SuspendRequests()
	}
	return nil
}

func handleSuspendRequest(ctx context.Context, m backend.Machine) error {
	handler, ok := m.(suspendRequestHandler)
	if !ok {
		return fmt.Errorf("machine cannot handle queued suspend requests: %w", errors.ErrUnsupported)
	}
	return handler.HandleSuspendRequest(ctx)
}

func suspend(ctx context.Context, m backend.Machine) error {
	if _, ok := m.(suspendRequestHandler); ok {
		return handleSuspendRequest(ctx, m)
	}
	suspender, ok := m.(backend.Suspender)
	if !ok {
		return fmt.Errorf("machine cannot suspend: %w", errors.ErrUnsupported)
	}
	return suspender.Suspend(ctx)
}

func logStatus(ctx context.Context, m backend.Machine, logger *slog.Logger) {
	if reporter, ok := m.(backend.StatusReporter); ok {
		if status, err := reporter.Status(ctx); err == nil {
			logger.Info("machine status", "state", status.State, "cid", status.CID, "pid", status.PID)
		}
	}
}

func sshCommand(mf *manifest.Manifest, cid int, remote []string) (*exec.Cmd, error) {
	renderer, err := manifest.NewTemplateRenderer(manifest.SSHTemplateProvider{
		CID: cid, User: mf.SSH.User, Destination: sshtools.VSockDestination(mf.SSH.User, cid),
	})
	if err != nil {
		return nil, err
	}
	argv, err := renderer.RenderArgv(mf.SSH.Argv)
	if err != nil {
		return nil, err
	}
	command, err := sshtools.NewCommand(sshtools.Config{Exec: argv, User: mf.SSH.User}, cid, remote)
	if err != nil {
		return nil, err
	}
	cmd := executor.Command(command.Path, command.Args, renderer.Env())
	cmd.Dir = mf.Paths.WorkingDir
	return cmd, nil
}

// ExitCode maps session errors onto CLI exit codes.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var commandErr *launch.CommandError
	if errors.As(err, &commandErr) && commandErr.ExitCode >= 0 {
		return commandErr.ExitCode
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	if errors.Is(err, context.Canceled) {
		return 130
	}
	return 1
}
