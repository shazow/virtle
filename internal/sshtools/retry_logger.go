package sshtools

import (
	"fmt"
	"log/slog"
)

// retryWarnThreshold is the number of transient SSH failures after which a
// single warning points the user at guest reachability and credentials.
const retryWarnThreshold = 5

// RetryLogger reports SSH connection retries once per phase, plus one warning
// once the retries stop looking like ordinary boot latency.
type RetryLogger struct {
	logger            *slog.Logger
	seen              map[RetryPhase]bool
	transientFailures int
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
	if l.transientFailures == retryWarnThreshold {
		l.logger.Warn(
			fmt.Sprintf("ssh exec failed %d times; ensure the guest is reachable and credentials are configured", retryWarnThreshold),
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
