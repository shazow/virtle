package runtime

import (
	"context"
	"log/slog"
	"net"

	"github.com/shazow/virtle/internal/control"
)

// serveControl starts serving listener and waits for startup.
func serveControl(ctx context.Context, listener net.Listener, server *control.Server, logger *slog.Logger) error {
	served := make(chan error, 1)
	go func() {
		err := server.Serve(listener)
		_ = server.Close()
		_ = listener.Close()
		served <- err
		if err != nil && ctx.Err() == nil && logger != nil {
			logger.Warn("control socket stopped", "err", err)
		}
	}()
	select {
	case <-server.Started():
		return nil
	case err := <-served:
		// Close may win before Serve registers its listener and signals Started.
		if err == nil {
			err = net.ErrClosed
		}
		return err
	}
}
