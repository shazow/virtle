package manager

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/shazow/virtle/internal/executor"
	"github.com/shazow/virtle/internal/qga"
)

type Info struct {
	ProcessList string
}

func (m *manager) collectGuestInfo(ctx context.Context, guestAgentTimeout time.Duration, socketPath string, watchers executor.Group) (Info, error) {
	if socketPath == "" {
		return Info{}, fmt.Errorf("guest agent socket is not configured")
	}

	timeout := 2 * m.effectiveQMPCommandTimeout()
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	infoCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client, err := m.waitForGuestAgent(infoCtx, guestAgentTimeout, socketPath, watchers)
	if err != nil {
		return Info{}, err
	}
	defer client.Disconnect()

	status, err := m.runGuestCommandStatus(infoCtx, client, guestAgentTimeout, "ps", guestPSPath, []string{"-eo", "user=,comm="}, "process list")
	if err != nil {
		return Info{}, err
	}
	if status.ExitCode != 0 {
		return Info{}, fmt.Errorf("ps %q exited with status %d%s", "process list", status.ExitCode, qga.ExecOutputSuffix(status))
	}

	return Info{ProcessList: qga.FormatProcessListExecData(status.OutData)}, nil
}

func (m *manager) printGuestInfo(ctx context.Context, guestAgentTimeout time.Duration, socketPath string, watchers executor.Group) {
	info, err := m.collectGuestInfo(ctx, guestAgentTimeout, socketPath, watchers)
	if err != nil {
		m.logger.Info("guest info failed", "err", err)
		return
	}

	m.logger.Info("guest info")
	processList := strings.TrimRight(info.ProcessList, "\n")
	if processList != "" {
		fmt.Fprintln(m.outputWriter(), processList)
	}
}
