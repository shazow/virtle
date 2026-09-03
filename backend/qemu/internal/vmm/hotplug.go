package vmm

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/shazow/virtle/backend/qemu/internal/qga"
	"github.com/shazow/virtle/internal/executor"
)

type managedProcessStarter struct {
	m *manager
}

func (s managedProcessStarter) Start(ctx context.Context, cmd *exec.Cmd) (*executor.Process, error) {
	return s.m.startManagedProcess(cmd)
}

func (s managedProcessStarter) Stop(process *executor.Process) error {
	if process == nil {
		return nil
	}
	// Cleanup of a failed attach must run to completion even when the attach
	// ctx is already canceled.
	return process.Stop(context.Background())
}

type socketReadinessWaiter struct {
	m *manager
}

func (w socketReadinessWaiter) Wait(ctx context.Context, stage string, socketPaths []string, process *executor.Process) error {
	// NewGroup skips a nil process, so a helper-less device waits on nothing.
	return w.m.waitForSockets(ctx, stage, socketPaths, executor.NewGroup(process))
}

type guestCommandRunner struct {
	m *manager
}

func (g guestCommandRunner) Run(ctx context.Context, command []string) error {
	if len(command) == 0 {
		return nil
	}
	if command[0] == "" {
		return fmt.Errorf("guest command path is required")
	}
	socketPath, err := g.m.launchManifest.ResolvedGuestAgentSocketPath()
	if err != nil {
		return err
	}
	client, err := g.m.waitForGuestAgent(ctx, socketPath, executor.Group{})
	if err != nil {
		return err
	}
	defer client.Disconnect()
	status, err := g.m.runGuestCommandStatus(ctx, client, filepath.Base(command[0]), command[0], command[1:], nil, strings.Join(command, " "))
	if err != nil {
		return err
	}
	if status.ExitCode != 0 {
		return fmt.Errorf("guest command %q exited with status %d%s", strings.Join(command, " "), status.ExitCode, qga.ExecOutputSuffix(status))
	}
	return nil
}
