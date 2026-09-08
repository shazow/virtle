// Package session adapts QEMU suspend persistence and SSH provisioning to
// the shared CLI session lifecycle. It is internal to the virtle module.
package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/backend/qemu/internal/launch"
	"github.com/shazow/virtle/backend/qemu/internal/qga"
	"github.com/shazow/virtle/internal/executor"
	"github.com/shazow/virtle/internal/manifest"
	shared "github.com/shazow/virtle/internal/session"
	"github.com/shazow/virtle/internal/sessionbridge"
	"github.com/shazow/virtle/internal/sshtools"
	"github.com/shazow/virtle/vm"
)

const (
	guestShellPath      = "/bin/sh"
	sshRetryOutputDelay = 250 * time.Millisecond
)

type Options = shared.Options

func ExitCode(err error) int { return shared.ExitCode(err) }

func Hooks() shared.Hooks {
	return shared.Hooks{Start: start, RunSSH: runSSH, SSHCommandHint: launch.BuildSSHCommandHint}
}
func Run(ctx context.Context, b backend.Backend, spec *vm.Spec, mf *manifest.Manifest, opts Options) error {
	return shared.RunWithHooks(ctx, b, spec, mf, opts, Hooks())
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
			return shared.WaitForSSHProcess(ctx, m, bridge, process, signals, logger)
		},
		WaitForRetry: func(ctx context.Context, _ executor.Group) error {
			return shared.WaitForSSHRetry(ctx, m, bridge, mf.SSH.RetryDelay, signals, logger)
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
