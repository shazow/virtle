package vmm

import (
	"io"
	"log/slog"
)

var discardLogger = slog.New(slog.DiscardHandler)

func (m *manager) backgroundWriter() io.Writer {
	if m != nil && m.backgroundOutput != nil {
		return m.backgroundOutput
	}
	return io.Discard
}

func (m *manager) sshLifecycleLogger() *slog.Logger {
	if m != nil && m.sshLogger != nil {
		return m.sshLogger
	}
	if m != nil && m.logger != nil {
		return m.logger
	}
	return discardLogger
}
