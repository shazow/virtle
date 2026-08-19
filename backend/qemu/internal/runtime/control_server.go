package runtime

import (
	"context"
	"log/slog"

	"github.com/shazow/virtle/internal/control"
)

func StartControl(ctx context.Context, socketPath string, router *control.Router, logger *slog.Logger) (*control.Server, error) {
	if socketPath == "" {
		return nil, nil
	}
	listener, err := control.Listen(socketPath)
	if err != nil {
		return nil, err
	}
	server, err := control.NewServer(router)
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	go func() {
		if err := server.Serve(listener); err != nil && ctx.Err() == nil && logger != nil {
			logger.Warn("control socket stopped", "err", err)
		}
	}()
	return server, nil
}
