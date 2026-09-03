package launch

import (
	"context"
	"io"
	"log/slog"
	"time"

	shellquote "github.com/kballard/go-shellquote"
	"github.com/shazow/virtle/internal/executor"
	"github.com/shazow/virtle/internal/sshtools"
)

type SSHAutoprovisionKey struct {
	IdentityFile  string
	PublicKeyFile string
	AuthorizedKey string
}

type SSHSession struct {
	Plan                   *Plan
	Runner                 Runner
	Logger                 *slog.Logger
	Stdin                  io.Reader
	Stdout                 io.Writer
	Stderr                 io.Writer
	RetryOutputRevealDelay time.Duration

	AddProcesses  func(...*executor.Process)
	RemoveProcess func(*executor.Process) bool
	Watchers      func() executor.Group
	RecordTimer   func(TimerEvent, time.Time)
	Wait          func(context.Context, *executor.Process, executor.Group) error
	WaitForRetry  func(context.Context, executor.Group) error
	EnsureKey     func() (SSHAutoprovisionKey, error)
	InstallKey    func(context.Context, SSHAutoprovisionKey, executor.Group) error
	Established   func() error
	Now           func() time.Time
}

func RunSSHSession(ctx context.Context, session SSHSession) error {
	plan := session.Plan
	launchManifest := plan.Manifest
	argv := append([]string(nil), launchManifest.SSH.Argv...)
	sessionLogger := session.Logger
	if sessionLogger == nil {
		sessionLogger = slog.New(slog.DiscardHandler)
	}
	stdout := session.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	stderrOutput := session.Stderr
	if stderrOutput == nil {
		stderrOutput = io.Discard
	}
	retryLog := sshtools.NewRetryLogger(sessionLogger)
	provisioned := false

	for {
		stderr := sshtools.NewRetryOutput(stderrOutput, false, session.RetryOutputRevealDelay)
		attemptStarted := sshSessionNow(session)
		if session.RecordTimer != nil {
			session.RecordTimer(TimerSSHAttempt, attemptStarted)
		}
		cmd, err := buildSSHCommandWithArgv(launchManifest, plan.CID, plan.RemoteCommand, argv)
		if err != nil {
			return WrapStage("active session", err)
		}
		sessionLogger.Info("ssh command", "command", shellquote.Join(cmd.Args...))
		cmd.Stdin = session.Stdin
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		started, err := session.Runner.Start(cmd)
		if err != nil {
			return WrapStage("active session", err)
		}
		watchers := executor.Group{}
		if session.Watchers != nil {
			watchers = session.Watchers()
		}
		if session.AddProcesses != nil {
			session.AddProcesses(started)
		}
		if session.RecordTimer != nil {
			session.RecordTimer(TimerSSHStarted, attemptStarted)
		}
		if session.Established != nil {
			if err := session.Established(); err != nil {
				_ = started.Stop(context.Background())
				return WrapStage("active session", err)
			}
		}

		err = session.Wait(ctx, started, watchers)
		stderrText := stderr.String()
		if err == nil {
			stderr.Flush()
			return nil
		}
		failure := sshtools.ClassifyFailure(err, stderrText)
		if failure == sshtools.FailureTransient {
			stderr.Suppress()
			retryLog.Log(err, stderrText)
			if session.RemoveProcess != nil {
				session.RemoveProcess(started)
			}
			if session.WaitForRetry != nil {
				if waitErr := session.WaitForRetry(ctx, watchers); waitErr != nil {
					return waitErr
				}
			}
			continue
		}
		if launchManifest.SSH.Autoprovision && !provisioned && failure == sshtools.FailureAuthentication {
			stderr.Suppress()
			if session.RemoveProcess != nil {
				session.RemoveProcess(started)
			}
			sessionLogger.Info("ssh authentication failed; autoprovisioning a key", "state_dir", launchManifest.ResolvedPersistenceStateDir(), "user", launchManifest.SSH.User)
			key, keyErr := session.EnsureKey()
			if keyErr != nil {
				return WrapStage("ssh autoprovision", keyErr)
			}
			if installErr := session.InstallKey(ctx, key, watchers); installErr != nil {
				return installErr
			}
			sessionLogger.Info("installed autoprovisioned ssh key; retrying ssh", "identity_file", key.IdentityFile, "public_key_file", key.PublicKeyFile)
			argv = (sshtools.Config{Exec: launchManifest.SSH.Argv, User: launchManifest.SSH.User}).WithIdentity(key.IdentityFile).Exec
			provisioned = true
			continue
		}
		stderr.Flush()
		return err
	}
}

func sshSessionNow(session SSHSession) time.Time {
	if session.Now != nil {
		return session.Now()
	}
	return time.Now()
}
