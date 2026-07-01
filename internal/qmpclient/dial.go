package qmpclient

import (
	"context"
	"fmt"
	"time"
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
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}

		if retry.Check != nil {
			if err := retry.Check(); err != nil {
				return nil, err
			}
		}

		client, err := dialer.Dial(ctx, retry.SocketPath, retry.Timeout)
		if err == nil {
			return client, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		timer.Reset(retry.RetryDelay)
	}
}
