package launch

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/shazow/virtle/internal/executor"
)

type SocketWait struct {
	Stage        string
	SocketPaths  []string
	SocketWaiter SocketWaiter
	PollDelay    time.Duration
	Watchers     executor.Group
}

func WaitForSockets(ctx context.Context, wait SocketWait) error {
	if wait.PollDelay <= 0 {
		wait.PollDelay = time.Second
	}
	waitCtx, cancelWait := context.WithCancel(ctx)
	defer cancelWait()

	errCh := make(chan error, 1)
	go func() {
		if wait.SocketWaiter == nil {
			errCh <- nil
			return
		}
		errCh <- wait.SocketWaiter.Wait(waitCtx, wait.SocketPaths)
	}()

	ticker := time.NewTicker(wait.PollDelay)
	defer ticker.Stop()

	for {
		select {
		case err := <-errCh:
			if err != nil {
				return WrapStage(wait.Stage, err)
			}
			return nil
		case <-ticker.C:
			if err := firstUnexpectedExit(wait.Stage, wait.Watchers); err != nil {
				return err
			}
		case <-ctx.Done():
			return WrapStage(wait.Stage, ctx.Err())
		}
	}
}

// waitForClient waits for wait's socket to appear, then dials it until the
// dial succeeds or a watched process exits. subject names the socket in the
// not-configured error; the dial callback receives the watcher check to run
// between attempts.
func waitForClient[C any](ctx context.Context, wait SocketWait, subject string, dial func(ctx context.Context, check func() error) (C, error)) (C, error) {
	var zero C
	if wait.SocketWaiter == nil {
		return zero, fmt.Errorf("%s socket waiter is not configured", subject)
	}
	if err := WaitForSockets(ctx, wait); err != nil {
		return zero, err
	}
	client, err := dial(ctx, func() error {
		return firstUnexpectedExit(wait.Stage, wait.Watchers)
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return zero, WrapStage(wait.Stage, err)
		}
		return zero, err
	}
	return client, nil
}
