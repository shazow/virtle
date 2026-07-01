package manager

import (
	"context"
	"io"
	"net"
	"time"

	"github.com/shazow/virtle/internal/executor"
	"github.com/shazow/virtle/internal/manager/launch"
	"github.com/shazow/virtle/internal/readiness"
)

const (
	defaultSSHReadyTimeout = 2 * time.Minute
	sshReadyTimeoutEnv     = "VIRTIE_SSH_READY_TIMEOUT"
	SSHReadyToken          = launch.SSHReadyToken
)

type unixSSHReadyDialer struct{}

func configuredSSHReadyTimeout() time.Duration {
	return readiness.TimeoutFromEnv(sshReadyTimeoutEnv, defaultSSHReadyTimeout)
}

func (d *unixSSHReadyDialer) Dial(ctx context.Context, socketPath string, timeout time.Duration) (io.ReadCloser, error) {
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	conn, err := (&net.Dialer{}).DialContext(dialCtx, "unix", socketPath)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

func (m *manager) waitForSSHReady(ctx context.Context, socketPath string, watchers executor.Group) error {
	timeout := m.effectiveSSHReadyTimeout()
	dialer := m.sshReadyDialer
	if dialer == nil {
		dialer = &unixSSHReadyDialer{}
	}
	return launch.WaitForSSHReady(ctx, launch.SSHReadyWait{
		Stage:        "vm startup",
		SocketPath:   socketPath,
		Token:        SSHReadyToken,
		Timeout:      timeout,
		PollDelay:    defaultSocketPollInterval,
		SocketWaiter: m.socketWaiter,
		Dialer:       dialer,
		Watchers:     watchers,
	})
}

func (m *manager) effectiveSSHReadyTimeout() time.Duration {
	if m.sshReadyTimeout > 0 {
		return m.sshReadyTimeout
	}
	return defaultSSHReadyTimeout
}
