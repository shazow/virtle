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
	wrapStage     func(stage string, err error) error
	Now           func() time.Time
}

func RunSSHSession(ctx context.Context, session SSHSession) error {
	launchManifest := session.Plan.Manifest
	argv := append([]string(nil), launchManifest.SSH.Argv...)
	sessionLogger, stdout, stderrOutput := sshSessionDefaults(session)
	retryLog := sshtools.NewRetryLogger(sessionLogger)
	provisioned := false

	for {
		stderr := sshtools.NewRetryOutput(stderrOutput, false, session.RetryOutputRevealDelay)
		started, watchers, err := startSSHAttempt(session, argv, stdout, stderr, sessionLogger)
		if err != nil {
			return err
		}

		err = session.Wait(ctx, started, watchers)
		stderrText := stderr.String()
		if err == nil {
			stderr.Flush()
			return nil
		}
		if sshtools.ClassifyFailure(err, stderrText) == sshtools.FailureTransient {
			if waitErr := retrySSHTransient(ctx, session, stderr, retryLog, err, stderrText, started, watchers); waitErr != nil {
				return waitErr
			}
			continue
		}
		if launchManifest.SSH.Autoprovision && !provisioned && sshtools.ClassifyFailure(err, stderrText) == sshtools.FailureAuthentication {
			newArgv, provisionErr := autoprovisionSSHSessionKey(ctx, session, stderr, sessionLogger, started, watchers)
			if provisionErr != nil {
				return provisionErr
			}
			argv = newArgv
			provisioned = true
			continue
		}
		stderr.Flush()
		return err
	}
}

func sshSessionDefaults(session SSHSession) (*slog.Logger, io.Writer, io.Writer) {
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
	return sessionLogger, stdout, stderrOutput
}

func startSSHAttempt(session SSHSession, argv []string, stdout, stderr io.Writer, sessionLogger *slog.Logger) (*executor.Process, executor.Group, error) {
	plan := session.Plan
	attemptStarted := sshSessionNow(session)
	if session.RecordTimer != nil {
		session.RecordTimer(TimerSSHAttempt, attemptStarted)
	}
	cmd, err := buildSSHCommandWithArgv(plan.Manifest, plan.CID, plan.RemoteCommand, argv)
	if err != nil {
		return nil, executor.Group{}, wrapSSHSessionStage(session, "active session", err)
	}
	sessionLogger.Info("ssh command", "command", shellquote.Join(cmd.Args...))
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	started, err := session.Runner.Start(cmd)
	if err != nil {
		return nil, executor.Group{}, wrapSSHSessionStage(session, "active session", err)
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
	return started, watchers, nil
}

func retrySSHTransient(ctx context.Context, session SSHSession, stderr *sshtools.RetryOutput, retryLog *sshtools.RetryLogger, err error, stderrText string, started *executor.Process, watchers executor.Group) error {
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
	return nil
}

func autoprovisionSSHSessionKey(ctx context.Context, session SSHSession, stderr *sshtools.RetryOutput, sessionLogger *slog.Logger, started *executor.Process, watchers executor.Group) ([]string, error) {
	launchManifest := session.Plan.Manifest
	stderr.Suppress()
	if session.RemoveProcess != nil {
		session.RemoveProcess(started)
	}
	sessionLogger.Info("ssh authentication failed; autoprovisioning a key", "state_dir", launchManifest.ResolvedPersistenceStateDir(), "user", launchManifest.SSH.User)
	key, keyErr := session.EnsureKey()
	if keyErr != nil {
		return nil, wrapSSHSessionStage(session, "ssh autoprovision", keyErr)
	}
	if installErr := session.InstallKey(ctx, key, watchers); installErr != nil {
		return nil, installErr
	}
	sessionLogger.Info("installed autoprovisioned ssh key; retrying ssh", "identity_file", key.IdentityFile, "public_key_file", key.PublicKeyFile)
	return (sshtools.Config{Exec: launchManifest.SSH.Argv, User: launchManifest.SSH.User}).WithIdentity(key.IdentityFile).Exec, nil
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
