package launch

import (
	"context"
	"time"

	"github.com/shazow/virtle/backend/qemu/internal/qga"
	"github.com/shazow/virtle/internal/executor"
)

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
	return waitForClient(ctx, SocketWait{
		Stage:        stage,
		SocketPaths:  []string{wait.SocketPath},
		SocketWaiter: wait.SocketWaiter,
		PollDelay:    wait.PollDelay,
		Watchers:     wait.Watchers,
	}, "guest agent", func(ctx context.Context, check func() error) (qga.Client, error) {
		return qga.DialWithRetry(ctx, wait.Dialer, qga.DialRetry{
			SocketPath:     wait.SocketPath,
			ConnectTimeout: wait.ConnectTimeout,
			RetryDelay:     wait.RetryDelay,
			Check:          check,
		})
	})
}
