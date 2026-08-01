// Package qmpwire shares line-delimited JSON socket helpers between the QMP
// and guest-agent clients, which speak the same wire framing over unix
// sockets.
package qmpwire

import (
	"context"
	"errors"
	"net"
	"time"
)

// AppendDelimiter appends the newline delimiter QEMU's JSON socket protocols
// expect, without mutating the input.
func AppendDelimiter(command []byte) []byte {
	if len(command) > 0 && command[len(command)-1] == '\n' {
		return command
	}
	return append(append([]byte(nil), command...), '\n')
}

// IsTimeout reports whether err is a network timeout.
func IsTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// Retry configures DialWithRetry for a client type C.
type Retry[C any] struct {
	// Dial attempts one connection.
	Dial func(ctx context.Context) (C, error)
	// Probe optionally verifies a fresh connection (e.g. a ping); on failure
	// Close is called and the dial is retried.
	Probe func(C) error
	// Close releases a connection whose probe failed.
	Close func(C)
	// Check runs before each attempt; a non-nil error aborts the retry loop
	// (e.g. a watched process exited).
	Check func() error
	// Delay is the wait between attempts.
	Delay time.Duration
}

// DialWithRetry dials until a connection (passing Probe, when set) is
// established, Check fails, or ctx is canceled.
func DialWithRetry[C any](ctx context.Context, retry Retry[C]) (C, error) {
	var zero C
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		case <-timer.C:
		}

		if retry.Check != nil {
			if err := retry.Check(); err != nil {
				return zero, err
			}
		}

		client, err := retry.Dial(ctx)
		if err == nil {
			if retry.Probe == nil {
				return client, nil
			}
			if err := retry.Probe(client); err == nil {
				return client, nil
			}
			if retry.Close != nil {
				retry.Close(client)
			}
		}
		if ctx.Err() != nil {
			return zero, ctx.Err()
		}

		timer.Reset(retry.Delay)
	}
}
