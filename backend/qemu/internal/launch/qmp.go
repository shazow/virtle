package launch

import (
	"context"
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
	return waitForClient(ctx, SocketWait{
		Stage:        stage,
		SocketPaths:  []string{wait.SocketPath},
		SocketWaiter: wait.SocketWaiter,
		PollDelay:    wait.PollDelay,
		Watchers:     wait.Watchers,
	}, "qmp", func(ctx context.Context, check func() error) (qmpclient.Client, error) {
		return qmpclient.DialWithRetry(ctx, wait.Dialer, qmpclient.DialRetry{
			SocketPath:     wait.SocketPath,
			ConnectTimeout: wait.ConnectTimeout,
			RetryDelay:     wait.RetryDelay,
			Check:          check,
		})
	})
}
