package launch

import (
	"context"
	"fmt"
	"time"

	"github.com/shazow/virtle/internal/executor"
	"github.com/shazow/virtle/internal/readiness"
)

const SSHReadyToken = "SSH-READY"

type SSHReadyWait struct {
	Stage        string
	SocketPath   string
	Token        string
	Timeout      time.Duration
	PollDelay    time.Duration
	SocketWaiter SocketWaiter
	Dialer       SSHReadyDialer
	Watchers     executor.Group
}

func WaitForSSHReady(ctx context.Context, wait SSHReadyWait) error {
	stage := wait.Stage
	if stage == "" {
		stage = "vm startup"
	}
	token := wait.Token
	if token == "" {
		token = SSHReadyToken
	}

	readyCtx, cancel := context.WithTimeout(ctx, wait.Timeout)
	defer cancel()

	if err := WaitForSockets(readyCtx, SocketWait{
		Stage:        stage,
		SocketPaths:  []string{wait.SocketPath},
		SocketWaiter: wait.SocketWaiter,
		PollDelay:    wait.PollDelay,
		Watchers:     wait.Watchers,
	}); err != nil {
		if readyCtx.Err() != nil {
			return wrapSSHReadyWait(stage, readyCtx.Err())
		}
		return err
	}

	if wait.Dialer == nil {
		return wrapStage(stage, fmt.Errorf("ssh readiness dialer is not configured"))
	}
	reader, err := wait.Dialer.Dial(readyCtx, wait.SocketPath, wait.Timeout)
	if err != nil {
		if readyCtx.Err() != nil {
			return wrapSSHReadyWait(stage, readyCtx.Err())
		}
		return wrapStage(stage, fmt.Errorf("connect ssh readiness socket %q: %w", wait.SocketPath, err))
	}
	defer reader.Close()

	errCh := make(chan error, 1)
	go func() {
		errCh <- readiness.ReadToken(reader, token)
	}()

	pollDelay := wait.PollDelay
	if pollDelay <= 0 {
		pollDelay = time.Second
	}
	ticker := time.NewTicker(pollDelay)
	defer ticker.Stop()

	for {
		select {
		case err := <-errCh:
			if err != nil {
				return wrapStage(stage, err)
			}
			return nil
		case <-ticker.C:
			if err := firstUnexpectedExit(stage, wait.Watchers); err != nil {
				return err
			}
		case <-readyCtx.Done():
			return wrapSSHReadyWait(stage, readyCtx.Err())
		}
	}
}

func wrapSSHReadyWait(stage string, err error) error {
	return wrapStage(stage, fmt.Errorf("wait for ssh readiness: %w", err))
}
