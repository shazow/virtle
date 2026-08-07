package qga

import (
	"context"
	"fmt"
	"time"

	"github.com/shazow/virtle/internal/qmpwire"
)

// DialRetry configures guest-agent dial retry behavior. The readiness ping is
// bounded by the transport's RPC timeout.
type DialRetry struct {
	SocketPath     string
	ConnectTimeout time.Duration
	RetryDelay     time.Duration
	Check          func() error
}

// DialWithRetry dials until a guest-agent client connects and responds to ping.
func DialWithRetry(ctx context.Context, dialer Dialer, retry DialRetry) (Client, error) {
	if dialer == nil {
		return nil, fmt.Errorf("guest agent dialer is not configured")
	}
	return qmpwire.DialWithRetry(ctx, qmpwire.Retry[Client]{
		Dial: func(ctx context.Context) (Client, error) {
			return dialer.Dial(ctx, retry.SocketPath, retry.ConnectTimeout)
		},
		Probe: func(client Client) error {
			return client.Ping(ctx)
		},
		Close: func(client Client) {
			_ = client.Disconnect()
		},
		Check: retry.Check,
		Delay: retry.RetryDelay,
	})
}
