package balloon

import "log/slog"

var logger = slog.New(slog.DiscardHandler)

func SetLogger(l *slog.Logger) {
	logger = l
}
