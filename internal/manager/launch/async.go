package launch

import (
	"context"
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
				return wrapStage(wait.Stage, err)
			}
			return nil
		case <-ticker.C:
			if err := firstUnexpectedExit(wait.Stage, wait.Watchers); err != nil {
				return err
			}
		case <-ctx.Done():
			return wrapStage(wait.Stage, ctx.Err())
		}
	}
}
