// Package session adapts QEMU suspend persistence, SSH readiness, and SSH
// provisioning to the shared CLI session loop in internal/session. It is
// internal to the virtle module.
package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/backend/qemu/internal/launch"
	"github.com/shazow/virtle/backend/qemu/internal/qga"
	"github.com/shazow/virtle/internal/executor"
	"github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/internal/readiness"
	shared "github.com/shazow/virtle/internal/session"
	"github.com/shazow/virtle/internal/sshtools"
	"github.com/shazow/virtle/vm"
)

const (
	guestShellPath      = "/bin/sh"
	sshRetryOutputDelay = 250 * time.Millisecond

	// The guest's SSH readiness token, written to the readiness socket by
	// the virtle guest setup once sshd accepts connections.
	sshReadyToken          = "SSH-READY"
	sshReadyTimeoutEnv     = "VIRTLE_SSH_READY_TIMEOUT"
	defaultSSHReadyTimeout = 2 * time.Minute
)

// Hooks returns the QEMU adapters for the shared CLI session loop: saved
// suspend state selects resume, the guest's SSH-READY token gates the
// foreground session, and --ssh attaches over vsock with optional key
// autoprovisioning through the guest agent.
func Hooks() shared.Hooks {
	return shared.Hooks{Start: start, Ready: ready, RunSSH: runSSH, SSHCommandHint: launch.BuildSSHCommandHint}
}

func start(ctx context.Context, b backend.Backend, spec *vm.Spec, mf *manifest.Manifest, mode shared.ResumeMode) (backend.Machine, bool, error) {
	resume := mode == shared.ResumeForce
	if mode == shared.ResumeAuto {
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

// ready waits for the guest to write the SSH-READY token on the machine's
// readiness socket, when it reports one. VIRTLE_SSH_READY_TIMEOUT overrides
// the two-minute bound.
func ready(ctx context.Context, m backend.Machine) error {
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
	ctx, cancel := context.WithTimeout(ctx, readiness.TimeoutFromEnv(sshReadyTimeoutEnv, defaultSSHReadyTimeout))
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", status.Paths.ReadySocket)
	if err != nil {
		return fmt.Errorf("connect readiness socket: %w", err)
	}
	defer conn.Close()
	// ReadToken has no context of its own; closing the connection ends it.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if err := readiness.ReadToken(conn, sshReadyToken); err != nil {
		if cause := context.Cause(ctx); cause != nil {
			return fmt.Errorf("wait for SSH readiness: %w", cause)
		}
		return err
	}
	return nil
}

func runSSH(ctx context.Context, s *shared.Session) error {
	reporter, ok := s.Machine.(backend.StatusReporter)
	if !ok {
		return fmt.Errorf("machine cannot report its SSH destination: %w", errors.ErrUnsupported)
	}
	status, err := reporter.Status(ctx)
	if err != nil {
		return err
	}
	logger := s.Logger.With("package", "ssh")
	opts := s.Options
	plan := &launch.Plan{Manifest: s.Manifest, CID: status.CID, RemoteCommand: append([]string(nil), opts.RemoteCommand...)}
	return launch.RunSSHSession(ctx, launch.SSHSession{
		Plan:                   plan,
		Runner:                 &executor.Runner{Logger: logger},
		Logger:                 logger,
		Stdin:                  optionReader(opts.Stdin, os.Stdin),
		Stdout:                 optionWriter(opts.Stdout, os.Stdout),
		Stderr:                 optionWriter(opts.Stderr, os.Stderr),
		RetryOutputRevealDelay: sshRetryOutputDelay,
		Wait: func(ctx context.Context, process *executor.Process, _ executor.Group) error {
			return s.WaitProcess(ctx, process)
		},
		WaitForRetry: func(ctx context.Context, _ executor.Group) error {
			return s.WaitRetry(ctx, s.Manifest.SSH.RetryDelay)
		},
		EnsureKey: func() (launch.SSHAutoprovisionKey, error) {
			return ensureSSHKey(s.Manifest)
		},
		InstallKey: func(ctx context.Context, key launch.SSHAutoprovisionKey, _ executor.Group) error {
			return installSSHKey(ctx, s.Machine, s.Manifest, key)
		},
		Established: s.Established,
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
