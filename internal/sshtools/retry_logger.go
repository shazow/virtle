package sshtools

import "log/slog"

type RetryLogger struct {
	logger            *slog.Logger
	seen              map[RetryPhase]bool
	transientFailures int
	warned            bool
}

func NewRetryLogger(logger *slog.Logger) *RetryLogger {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &RetryLogger{
		logger: logger,
		seen:   make(map[RetryPhase]bool),
	}
}

func (l *RetryLogger) Log(err error, stderr string) {
	if l == nil {
		return
	}
	phase := RetryPhaseForFailure(err, stderr)
	if phase == RetryPhaseNone {
		return
	}
	l.transientFailures++
	if l.transientFailures == 5 && !l.warned {
		l.warned = true
		l.logger.Warn(
			"ssh exec failed 5 times; ensure the guest is reachable and credentials are configured",
			"ssh_failures",
			l.transientFailures,
		)
	}
	if !l.seen[phase] {
		l.seen[phase] = true
		switch phase {
		case RetryPhaseWaiting:
			l.logger.Info("waiting for ssh connection")
		case RetryPhaseConnecting:
			l.logger.Info("connecting ssh")
		}
	}
}
