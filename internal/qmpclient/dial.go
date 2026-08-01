package qmpclient

import (
	"context"
	"fmt"
	"time"

	"github.com/shazow/virtle/internal/qmpwire"
)

// DialRetry configures QMP dial retry behavior.
type DialRetry struct {
	SocketPath string
	Timeout    time.Duration
	RetryDelay time.Duration
	Check      func() error
}

// DialWithRetry dials until a QMP client connects or ctx is canceled.
func DialWithRetry(ctx context.Context, dialer Dialer, retry DialRetry) (Client, error) {
	if dialer == nil {
		return nil, fmt.Errorf("qmp dialer is not configured")
	}
	return qmpwire.DialWithRetry(ctx, qmpwire.Retry[Client]{
		Dial: func(ctx context.Context) (Client, error) {
			return dialer.Dial(ctx, retry.SocketPath, retry.Timeout)
		},
		Check: retry.Check,
		Delay: retry.RetryDelay,
	})
}
