package vmm

import (
	"context"
	"fmt"
	"strings"

	"github.com/shazow/virtle/backend/qemu/internal/qga"
	"github.com/shazow/virtle/internal/executor"
)

type Info struct {
	ProcessList string
}

func (m *manager) collectGuestInfo(ctx context.Context, socketPath string, watchers executor.Group) (Info, error) {
	if socketPath == "" {
		return Info{}, fmt.Errorf("guest agent socket is not configured")
	}

	infoCtx, cancel := context.WithTimeout(ctx, defaultGuestInfoTimeout)
	defer cancel()

	client, err := m.waitForGuestAgent(infoCtx, socketPath, watchers)
	if err != nil {
		return Info{}, err
	}
	defer client.Disconnect()

	status, err := m.runGuestCommandStatus(infoCtx, client, "ps", guestPSPath, []string{"-eo", "user=,comm="}, "process list")
	if err != nil {
		return Info{}, err
	}
	if status.ExitCode != 0 {
		return Info{}, fmt.Errorf("ps %q exited with status %d%s", "process list", status.ExitCode, qga.ExecOutputSuffix(status))
	}

	return Info{ProcessList: qga.FormatProcessListExecData(status.OutData)}, nil
}

func (m *manager) printGuestInfo(ctx context.Context, socketPath string, watchers executor.Group) {
	info, err := m.collectGuestInfo(ctx, socketPath, watchers)
	if err != nil {
		m.logger.Info("guest info failed", "err", err)
		return
	}

	processList := strings.TrimRight(info.ProcessList, "\n")
	if processList != "" {
		m.logger.Debug("guest info", "processes", processList)
	}
}
