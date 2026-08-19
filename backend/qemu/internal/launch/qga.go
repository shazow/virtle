package launch

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/shazow/virtle/backend/qemu/internal/qga"
	"github.com/shazow/virtle/internal/executor"
)

// GuestCommandNotStarted reports whether err means a guest command never
// started because the guest agent could not execute the program. The guest is
// unchanged in that case, so retrying with a different program path is safe.
func GuestCommandNotStarted(err error) bool {
	var startErr *qga.ExecStartError
	return errors.As(err, &startErr)
}

type GuestAgentWait struct {
	Stage          string
	SocketPath     string
	SocketWaiter   SocketWaiter
	Dialer         qga.Dialer
	ConnectTimeout time.Duration
	RetryDelay     time.Duration
	PollDelay      time.Duration
	Watchers       executor.Group
}

func WaitForGuestAgent(ctx context.Context, wait GuestAgentWait) (qga.Client, error) {
	stage := wait.Stage
	if stage == "" {
		stage = "guest agent"
	}
	if wait.SocketWaiter == nil {
		return nil, fmt.Errorf("guest agent socket waiter is not configured")
	}
	if err := WaitForSockets(ctx, SocketWait{
		Stage:        stage,
		SocketPaths:  []string{wait.SocketPath},
		SocketWaiter: wait.SocketWaiter,
		PollDelay:    wait.PollDelay,
		Watchers:     wait.Watchers,
	}); err != nil {
		return nil, err
	}

	client, err := qga.DialWithRetry(ctx, wait.Dialer, qga.DialRetry{
		SocketPath:     wait.SocketPath,
		ConnectTimeout: wait.ConnectTimeout,
		RetryDelay:     wait.RetryDelay,
		Check: func() error {
			return firstUnexpectedExit(stage, wait.Watchers)
		},
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, wrapStage(stage, err)
		}
		return nil, err
	}
	return client, nil
}
