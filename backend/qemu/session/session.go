// Package session implements the virtle CLI foreground loop over the public
// backend machine contract.
//
// It is an implementation detail of the virtle command: Run takes the
// module-internal resolved manifest, so the package is not callable from
// outside this module and its API carries no compatibility promise. It lives
// under backend/qemu rather than internal/ only because it needs QEMU
// launch helpers that the root command cannot import.
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
	"github.com/shazow/virtle/backend/qemu/internal/launch"
	"github.com/shazow/virtle/backend/qemu/internal/qga"
	"github.com/shazow/virtle/backend/qemu/internal/sessionbridge"
	"github.com/shazow/virtle/internal/executor"
	"github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/internal/readiness"
	"github.com/shazow/virtle/internal/sshtools"
	"github.com/shazow/virtle/vm"
)

const (
	defaultSSHReadyTimeout = 2 * time.Minute
	sshReadyTimeoutEnv     = "VIRTLE_SSH_READY_TIMEOUT"
	sshReadyToken          = "SSH-READY"
	guestShellPath         = "/bin/sh"
	sshRetryOutputDelay    = 250 * time.Millisecond
)

// Options configures a CLI session.
type Options struct {
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

// Run starts a machine and owns it until exit, suspend, or signal shutdown.
func Run(ctx context.Context, b backend.Backend, spec *vm.Spec, mf *manifest.Manifest, opts Options) (err error) {
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
	if _, err := launch.NormalizeResumeMode(launch.ResumeMode(opts.Resume)); err != nil {
		return err
	}

	runCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGTSTP, syscall.SIGUSR1)
	defer signal.Stop(signals)
	bridge := &sessionbridge.Bridge{}
	runCtx = sessionbridge.WithContext(runCtx, bridge)

	m, resumed, err := start(runCtx, b, spec, mf, opts.Resume)
	if err != nil {
		return err
	}
	if !resumed {
		if err := waitReady(runCtx, m, bridge, signals, sshLogger); err != nil {
			if launch.IsSavedSuspendExit(err) {
				return nil
			}
			return shutdownAfter(runCtx, m, err)
		}
		sshLogger.Info("guest is ready")
	}
	sessionLogger.Info("vm started; entering foreground session")
	err = foreground(runCtx, m, bridge, mf, opts, signals, sessionLogger, sshLogger)
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

// shutdownAfter tears m down after err, uncancelably, and reports both.
func shutdownAfter(ctx context.Context, m backend.Machine, err error) error {
	return errors.Join(err, m.Shutdown(context.WithoutCancel(ctx)))
}

// afterSuspend reports a suspend outcome. A saved-state exit needs no
// teardown (the machine is already down); any other failure shuts m down.
func afterSuspend(ctx context.Context, m backend.Machine, err error) error {
	if err == nil || launch.IsSavedSuspendExit(err) {
		return err
	}
	return shutdownAfter(ctx, m, err)
}

func waitReady(ctx context.Context, m backend.Machine, bridge *sessionbridge.Bridge, signals <-chan os.Signal, logger *slog.Logger) error {
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
		err := runSSH(ctx, m, bridge, mf, opts, signals, logger, sshLogger)
		if launch.IsSavedSuspendExit(err) {
			return err
		}
		return shutdownAfter(ctx, m, err)
	}

	if reporter, ok := m.(backend.StatusReporter); ok && len(mf.SSH.Argv) > 0 {
		status, err := reporter.Status(ctx)
		if err != nil {
			return shutdownAfter(ctx, m, err)
		}
		hint, err := launch.BuildSSHCommandHint(mf, status.CID)
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

func runSSH(ctx context.Context, m backend.Machine, bridge *sessionbridge.Bridge, mf *manifest.Manifest, opts Options, signals <-chan os.Signal, logger *slog.Logger, sshLogger *slog.Logger) error {
	reporter, ok := m.(backend.StatusReporter)
	if !ok {
		return fmt.Errorf("machine cannot report its SSH destination: %w", errors.ErrUnsupported)
	}
	status, err := reporter.Status(ctx)
	if err != nil {
		return err
	}
	runner := &executor.Runner{Logger: logger}
	plan := &launch.Plan{Manifest: mf, CID: status.CID, RemoteCommand: append([]string(nil), opts.RemoteCommand...)}
	return launch.RunSSHSession(ctx, launch.SSHSession{
		Plan:                   plan,
		Runner:                 runner,
		Logger:                 sshLogger,
		Stdin:                  optionReader(opts.Stdin, os.Stdin),
		Stdout:                 optionWriter(opts.Stdout, os.Stdout),
		Stderr:                 optionWriter(opts.Stderr, os.Stderr),
		RetryOutputRevealDelay: sshRetryOutputDelay,
		Wait: func(ctx context.Context, process *executor.Process, _ executor.Group) error {
			return waitForSSHProcess(ctx, m, bridge, process, signals, logger)
		},
		WaitForRetry: func(ctx context.Context, _ executor.Group) error {
			return waitForSSHRetry(ctx, m, bridge, mf.SSH.RetryDelay, signals, logger)
		},
		EnsureKey: func() (launch.SSHAutoprovisionKey, error) {
			return ensureSSHKey(mf)
		},
		InstallKey: func(ctx context.Context, key launch.SSHAutoprovisionKey, _ executor.Group) error {
			return installSSHKey(ctx, m, mf, key)
		},
		Established: bridge.Commit,
	})
}

func waitForSSHProcess(ctx context.Context, m backend.Machine, bridge *sessionbridge.Bridge, process *executor.Process, signals <-chan os.Signal, logger *slog.Logger) error {
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

func waitForSSHRetry(ctx context.Context, m backend.Machine, bridge *sessionbridge.Bridge, delay time.Duration, signals <-chan os.Signal, logger *slog.Logger) error {
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

func ensureSSHKey(mf *manifest.Manifest) (launch.SSHAutoprovisionKey, error) {
	key, err := (sshtools.KeyStore{
		Dir:     mf.ResolvedPersistenceStateDir(),
		Comment: "virtle-autoprovision-" + mf.Identity.HostName,
	}).Ensure()
	if err != nil {
		return launch.SSHAutoprovisionKey{}, err
	}
	return launch.SSHAutoprovisionKey{
		IdentityFile: key.IdentityFile, PublicKeyFile: key.PublicKeyFile, AuthorizedKey: key.AuthorizedKey,
	}, nil
}

func installSSHKey(ctx context.Context, m backend.Machine, mf *manifest.Manifest, key launch.SSHAutoprovisionKey) error {
	guest, err := m.RemoteControl()
	if err != nil {
		return sshAutoprovisionError(err)
	}
	defer guest.Close()
	plan := sshtools.NewAuthorizedKeysInstallPlan(mf.SSH.User, key.AuthorizedKey)
	run := func(ctx context.Context, subject string, path string, args []string) error {
		commandCtx, cancel := mf.GuestCommandContext(ctx)
		defer cancel()
		if err := guest.Run(commandCtx, &vm.GuestCmd{Path: path, Args: args, Env: []string{qga.InternalCommandPathEnv}}); err != nil {
			return fmt.Errorf("%s %q: %w", subject, plan.AuthorizedKeysPath, err)
		}
		return nil
	}
	installer := launch.ScriptGuestDirectoryInstaller(run)
	if err := launch.InstallGuestFileDirectory(ctx, installer, plan.AuthorizedKeysPath, plan.Owner, "0600"); err != nil {
		return sshAutoprovisionError(err)
	}
	if err := run(ctx, "chown", "chown", []string{plan.Owner, plan.SSHDir}); err != nil {
		return sshAutoprovisionError(err)
	}
	if err := run(ctx, "chmod", "chmod", []string{"0700", plan.SSHDir}); err != nil {
		return sshAutoprovisionError(err)
	}
	commandCtx, cancel := mf.GuestCommandContext(ctx)
	writer, err := guest.Create(commandCtx, plan.TempKeyPath, 0o600)
	if err == nil {
		_, err = io.WriteString(writer, plan.TempKeyText)
	}
	if writer != nil {
		err = errors.Join(err, writer.Close())
	}
	cancel()
	if err != nil {
		return sshAutoprovisionError(fmt.Errorf("write temporary authorized key: %w", err))
	}
	appendCommand := plan.AppendCommand(guestShellPath)
	if err := run(ctx, appendCommand.Name, appendCommand.Path, appendCommand.Args); err != nil {
		return sshAutoprovisionError(err)
	}
	if err := run(ctx, "chown", "chown", []string{plan.Owner, plan.AuthorizedKeysPath}); err != nil {
		return sshAutoprovisionError(err)
	}
	if err := run(ctx, "chmod", "chmod", []string{"0600", plan.AuthorizedKeysPath}); err != nil {
		return sshAutoprovisionError(err)
	}
	return nil
}

func sshAutoprovisionError(err error) error {
	return &launch.StageError{Stage: "ssh autoprovision", Err: err}
}

func optionReader(configured io.Reader, fallback io.Reader) io.Reader {
	if configured != nil {
		return configured
	}
	return fallback
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
	var commandErr *launch.CommandError
	if errors.As(err, &commandErr) && commandErr.ExitCode >= 0 {
		return commandErr.ExitCode
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
