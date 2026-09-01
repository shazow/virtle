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

	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/backend/qemu/internal/launch"
	"github.com/shazow/virtle/internal/executor"
	"github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/internal/readiness"
	"github.com/shazow/virtle/internal/sshtools"
	"github.com/shazow/virtle/vm"
)

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
	m, resumed, err := start(ctx, b, spec, mf, opts.Resume)
	if err != nil {
		return err
	}
	if !resumed {
		if err := waitReady(ctx, m); err != nil {
			return errors.Join(err, m.Shutdown(context.WithoutCancel(ctx)))
		}
	}
	return foreground(ctx, m, mf, opts, logger.With("package", "session"))
}

func start(ctx context.Context, b backend.Backend, spec *vm.Spec, mf *manifest.Manifest, mode string) (backend.Machine, bool, error) {
	if mode == "" {
		mode = "auto"
	}
	if mode != "no" && mode != "auto" && mode != "force" {
		return nil, false, fmt.Errorf("unsupported resume mode %q", mode)
	}
	resume := mode == "force"
	if mode == "auto" {
		saved, err := launch.HasSavedSuspendState(mf)
		if err != nil {
			return nil, false, err
		}
		resume = saved
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

func waitReady(ctx context.Context, m backend.Machine) error {
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
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", status.Paths.ReadySocket)
	if err != nil {
		return fmt.Errorf("connect readiness socket: %w", err)
	}
	defer conn.Close()
	errCh := make(chan error, 1)
	go func() { errCh <- readiness.ReadToken(conn, launch.SSHReadyToken) }()
	select {
	case err := <-errCh:
		return err
	case <-m.Done():
		return m.Err()
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func foreground(ctx context.Context, m backend.Machine, mf *manifest.Manifest, opts Options, logger *slog.Logger) error {
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGTSTP, syscall.SIGUSR1)
	defer signal.Stop(signals)

	var session *executor.Process
	if opts.SSH {
		if len(mf.SSH.Argv) == 0 {
			return errors.Join(fmt.Errorf("--ssh requires a non-empty manifest.ssh.exec"), m.Shutdown(context.WithoutCancel(ctx)))
		}
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
		case sig := <-signals:
			switch sig {
			case syscall.SIGUSR1:
				if reporter, ok := m.(backend.StatusReporter); ok {
					if status, err := reporter.Status(ctx); err == nil {
						logger.Info("machine status", "state", status.State, "cid", status.CID, "pid", status.PID)
					}
				}
			case syscall.SIGTSTP:
				suspender, ok := m.(backend.Suspender)
				if !ok {
					return errors.Join(fmt.Errorf("machine cannot suspend: %w", errors.ErrUnsupported), m.Shutdown(context.WithoutCancel(ctx)))
				}
				if session != nil {
					_ = session.Stop(context.Background())
				}
				if err := suspender.Suspend(ctx); err != nil {
					return errors.Join(err, m.Shutdown(context.WithoutCancel(ctx)))
				}
				return nil
			default:
				if session != nil {
					_ = session.Stop(context.Background())
				}
				return m.Shutdown(context.WithoutCancel(ctx))
			}
		case <-ctx.Done():
			if session != nil {
				_ = session.Stop(context.Background())
			}
			return errors.Join(context.Cause(ctx), m.Shutdown(context.WithoutCancel(ctx)))
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
