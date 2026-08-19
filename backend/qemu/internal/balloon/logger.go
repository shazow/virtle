package balloon

import "log/slog"

var logger = slog.New(slog.DiscardHandler)

func SetLogger(l *slog.Logger) {
	if l == nil {
		l = slog.New(slog.DiscardHandler)
	}
	logger = l
}
