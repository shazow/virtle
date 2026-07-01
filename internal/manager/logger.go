package manager

import (
	"io"
	"log/slog"
)

var logger = slog.New(slog.DiscardHandler)

func SetLogger(l *slog.Logger) {
	logger = l
}

func (m *manager) outputWriter() io.Writer {
	if m != nil && m.logWriter != nil {
		return m.logWriter
	}
	return io.Discard
}
