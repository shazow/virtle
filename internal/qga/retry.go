package qga

import (
	"context"
	"fmt"
	"time"

	"github.com/shazow/virtle/internal/qmpwire"
)

// probeTimeout bounds the readiness ping. The QGA chardev accepts
// connections before the in-guest agent is listening and discards the bytes,
// so an unanswered probe must fail fast to keep the retry loop (and its
// process-exit checks) responsive; the steady-state RPC bound stays with the
// session.
const probeTimeout = 2 * time.Second

// DialRetry configures guest-agent dial retry behavior. The readiness ping is
// bounded by probeTimeout.
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
			probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
			defer cancel()
			return client.Ping(probeCtx)
		},
		Close: func(client Client) {
			_ = client.Disconnect()
		},
		Check: retry.Check,
		Delay: retry.RetryDelay,
	})
}
