package vmm

import (
	"io"
	"log/slog"
)

var (
	logger                     = slog.New(slog.DiscardHandler)
	sshLogger                  = slog.New(slog.DiscardHandler)
	backgroundOutput io.Writer = io.Discard
)

func SetLogger(l *slog.Logger) {
	if l == nil {
		l = slog.New(slog.DiscardHandler)
	}
	logger = l
}

func SetSSHLogger(l *slog.Logger) {
	if l == nil {
		l = slog.New(slog.DiscardHandler)
	}
	sshLogger = l
}

func SetOutput(w io.Writer) {
	if w == nil {
		w = io.Discard
	}
	backgroundOutput = w
}

func (m *manager) outputWriter() io.Writer {
	if m != nil && m.logWriter != nil {
		return m.logWriter
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
	return slog.New(slog.DiscardHandler)
}
