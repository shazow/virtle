package launch

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/shazow/virtle/backend/qemu/internal/qmpclient"
	"github.com/shazow/virtle/internal/executor"
)

type QMPWait struct {
	Stage          string
	SocketPath     string
	SocketWaiter   SocketWaiter
	Dialer         qmpclient.Dialer
	ConnectTimeout time.Duration
	RetryDelay     time.Duration
	PollDelay      time.Duration
	Watchers       executor.Group
}

func WaitForQMP(ctx context.Context, wait QMPWait) (qmpclient.Client, error) {
	stage := wait.Stage
	if stage == "" {
		stage = "vm startup"
	}
	if wait.SocketWaiter == nil {
		return nil, fmt.Errorf("qmp socket waiter is not configured")
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

	client, err := qmpclient.DialWithRetry(ctx, wait.Dialer, qmpclient.DialRetry{
		SocketPath: wait.SocketPath,
		Timeout:    wait.ConnectTimeout,
		RetryDelay: wait.RetryDelay,
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
