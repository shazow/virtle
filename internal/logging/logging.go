// Package logging defines virtle's command-line logging policy.
package logging

import (
	"context"
	"io"
	"log/slog"
)

// LevelNotice reports a user-visible consequence of an operation. It sits
// between Info and Warn so normal command output can remain sparse without
// misclassifying successful outcomes as warnings.
const LevelNotice slog.Level = 2

// New returns a package-aware text logger for the requested verbosity. Named
// child loggers must carry a "package" attribute. Main, session, and ssh INFO
// records are enabled by -v; every package's DEBUG and INFO records are enabled
// by -vv. NOTICE and warnings are always enabled.
func New(w io.Writer, verbosity int) *slog.Logger {
	if w == nil {
		w = io.Discard
	}
	base := slog.NewTextHandler(w, &slog.HandlerOptions{
		Level: slog.LevelDebug,
		ReplaceAttr: func(_ []string, attr slog.Attr) slog.Attr {
			if attr.Key == slog.LevelKey {
				if level, ok := attr.Value.Any().(slog.Level); ok && level == LevelNotice {
					attr.Value = slog.StringValue("NOTICE")
				}
			}
			return attr
		},
	})
	return slog.New(&tierHandler{next: base, verbosity: verbosity})
}

// Notice logs a successful, user-visible consequence of an operation.
func Notice(logger *slog.Logger, message string, args ...any) {
	if logger == nil {
		return
	}
	logger.Log(context.Background(), LevelNotice, message, args...)
}

type tierHandler struct {
	next        slog.Handler
	verbosity   int
	packageName string
}

func (h *tierHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= h.minimumLevel() && h.next.Enabled(ctx, level)
}

func (h *tierHandler) Handle(ctx context.Context, record slog.Record) error {
	return h.next.Handle(ctx, record)
}

func (h *tierHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := *h
	clone.next = h.next.WithAttrs(attrs)
	for _, attr := range attrs {
		if attr.Key == "package" && attr.Value.Kind() == slog.KindString {
			clone.packageName = attr.Value.String()
		}
	}
	return &clone
}

func (h *tierHandler) WithGroup(name string) slog.Handler {
	clone := *h
	clone.next = h.next.WithGroup(name)
	return &clone
}

func (h *tierHandler) minimumLevel() slog.Level {
	if h.verbosity >= 2 {
		return slog.LevelDebug
	}
	if h.verbosity == 1 {
		switch h.packageName {
		case "main", "session", "ssh":
			return slog.LevelInfo
		}
	}
	return LevelNotice
}
