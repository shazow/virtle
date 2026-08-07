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
	Output                 io.Writer
	RetryOutputRevealDelay time.Duration

	AddProcesses  func(...*executor.Process)
	RemoveProcess func(*executor.Process) bool
	Watchers      func() executor.Group
	RecordTimer   func(TimerEvent, time.Time)
	Wait          func(context.Context, *executor.Process, executor.Group) error
	WaitForRetry  func(context.Context, executor.Group) error
	EnsureKey     func() (SSHAutoprovisionKey, error)
	InstallKey    func(context.Context, SSHAutoprovisionKey, executor.Group) error
	wrapStage     func(stage string, err error) error
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
	retryLog := sshtools.NewRetryLogger(sessionLogger)
	provisioned := false

	for {
		stderr := sshtools.NewRetryOutput(session.Output, false, session.RetryOutputRevealDelay)
		attemptStarted := sshSessionNow(session)
		if session.RecordTimer != nil {
			session.RecordTimer(TimerSSHAttempt, attemptStarted)
		}
		cmd, err := buildSSHCommandWithArgv(launchManifest, plan.CID, plan.RemoteCommand, argv)
		if err != nil {
			return wrapSSHSessionStage(session, "active session", err)
		}
		sessionLogger.Info("ssh command", "command", shellquote.Join(cmd.Args...))
		cmd.Stderr = stderr
		started, err := session.Runner.Start(cmd)
		if err != nil {
			return wrapSSHSessionStage(session, "active session", err)
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

		err = session.Wait(ctx, started, watchers)
		stderrText := stderr.String()
		if err == nil {
			stderr.Flush()
			return nil
		}
		if sshtools.ClassifyFailure(err, stderrText) == sshtools.FailureTransient {
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
		if launchManifest.SSH.Autoprovision && !provisioned && sshtools.ClassifyFailure(err, stderrText) == sshtools.FailureAuthentication {
			stderr.Suppress()
			if session.RemoveProcess != nil {
				session.RemoveProcess(started)
			}
			sessionLogger.Info("ssh authentication failed; autoprovisioning a key", "state_dir", launchManifest.ResolvedPersistenceStateDir(), "user", launchManifest.SSH.User)
			key, keyErr := session.EnsureKey()
			if keyErr != nil {
				return wrapSSHSessionStage(session, "ssh autoprovision", keyErr)
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

func wrapSSHSessionStage(session SSHSession, stage string, err error) error {
	if session.wrapStage != nil {
		return session.wrapStage(stage, err)
	}
	return wrapStage(stage, err)
}
