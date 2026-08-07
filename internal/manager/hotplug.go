package manager

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/shazow/virtle/internal/executor"
	controlpkg "github.com/shazow/virtle/internal/manager/control"
	"github.com/shazow/virtle/internal/manager/launch"
	"github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/internal/qga"
)

func Hotplug(ctx context.Context, manifest *manifest.Manifest, id string, detach bool) error {
	m := newManager()
	m.launchManifest = manifest
	return m.hotplug(ctx, id, detach)
}

func (m *manager) hotplug(ctx context.Context, id string, detach bool) error {
	launchManifest := m.launchManifest
	if err := launchManifest.Validate(); err != nil {
		return &launch.StageError{Stage: "preflight", Err: err}
	}
	controlSocketPath, err := launchManifest.ResolvedControlSocketPath()
	if err != nil {
		return &launch.StageError{Stage: "preflight", Err: err}
	}
	if controlSocketPath == "" {
		return &launch.StageError{Stage: "control hotplug", Err: fmt.Errorf("control socket path is not configured")}
	}
	_, err = controlpkg.Dial(controlSocketPath).Hotplug(ctx, controlpkg.HotplugRequest{ID: id, Detach: detach})
	if err != nil {
		return &launch.StageError{Stage: "control hotplug", Err: err}
	}
	return nil
}

type managedProcessStarter struct {
	m *manager
}

func (s managedProcessStarter) Start(ctx context.Context, cmd *exec.Cmd) (*executor.Process, error) {
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	proc, err := s.m.startManagedProcess(cmd)
	if err != nil {
		return nil, err
	}
	return proc, nil
}

func (s managedProcessStarter) Stop(process *executor.Process) error {
	if process == nil {
		return nil
	}
	return process.Stop(s.m.shutdownDelay)
}

func (s managedProcessStarter) SignalPIDGroup(pid int, signal syscall.Signal) error {
	return executor.SignalProcessGroup(pid, signal)
}

type socketReadinessWaiter struct {
	m *manager
}

func (w socketReadinessWaiter) Wait(ctx context.Context, stage string, socketPaths []string, process *executor.Process) error {
	if process != nil {
		return w.m.waitForSockets(ctx, stage, socketPaths, executor.NewGroup(process))
	}
	return w.m.waitForSockets(ctx, stage, socketPaths, executor.Group{})
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
	ctx, cancel := g.m.launchManifest.GuestCommandContext(ctx)
	defer cancel()
	status, err := g.m.runGuestCommandStatus(ctx, client, filepath.Base(command[0]), command[0], command[1:], strings.Join(command, " "))
	if err != nil {
		return err
	}
	if status.ExitCode != 0 {
		return fmt.Errorf("guest command %q exited with status %d%s", strings.Join(command, " "), status.ExitCode, qga.ExecOutputSuffix(status))
	}
	return nil
}
