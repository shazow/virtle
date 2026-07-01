package qga

import (
	"context"
	"fmt"
	"time"
)

// DialRetry configures guest-agent dial retry behavior.
type DialRetry struct {
	SocketPath     string
	ConnectTimeout time.Duration
	CommandTimeout time.Duration
	RetryDelay     time.Duration
	Check          func() error
}

// DialWithRetry dials until a guest-agent client connects and responds to ping.
func DialWithRetry(ctx context.Context, dialer Dialer, retry DialRetry) (Client, error) {
	if dialer == nil {
		return nil, fmt.Errorf("guest agent dialer is not configured")
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

		client, err := dialer.Dial(ctx, retry.SocketPath, retry.ConnectTimeout)
		if err == nil {
			if err := client.Ping(retry.CommandTimeout); err == nil {
				return client, nil
			}
			_ = client.Disconnect()
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		timer.Reset(retry.RetryDelay)
	}
}
