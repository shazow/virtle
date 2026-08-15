package vmm

import (
	"context"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/shazow/virtle/backend/qemu/internal/launch"
	controlpkg "github.com/shazow/virtle/internal/control"
	"github.com/shazow/virtle/internal/manifest"
)

const defaultLaunchSignalTimeout = 5 * time.Second

// Suspend saves the running VM state to disk and exits the launch process.
func Suspend(ctx context.Context, manifest *manifest.Manifest) error {
	m := newManager()
	m.launchManifest = manifest
	return m.suspend(ctx)
}

func (m *manager) suspend(ctx context.Context) error {
	manifest := m.launchManifest
	controlSocketPath, err := manifest.ResolvedControlSocketPath()
	if err == nil && controlSocketPath != "" {
		resp, err := controlpkg.Dial(controlSocketPath).Suspend(ctx, controlpkg.SuspendRequest{})
		if err == nil {
			if resp.Saved {
				timeout := m.effectiveSuspendSignalTimeout()
				waitCtx, cancel := context.WithTimeout(ctx, timeout)
				defer cancel()
				if err := launch.WaitForLaunchExited(waitCtx, manifest, timeout); err != nil {
					return err
				}
				return nil
			}
			return &launch.StageError{Stage: "qmp suspend", Err: fmt.Errorf("launch process did not save VM state")}
		}
		if !controlpkg.IsSocketUnavailable(err) {
			return &launch.StageError{Stage: "control suspend", Err: err}
		}
	}

	pid, err := m.launchPID()
	if err != nil {
		if saved, stateErr := launch.HasSavedSuspendState(manifest); stateErr != nil {
			return &launch.StageError{Stage: "qmp suspend", Err: stateErr}
		} else if saved {
			return nil
		}
		return err
	}

	if err := m.signalLaunchPID(pid, syscall.SIGTSTP); err != nil {
		return &launch.StageError{Stage: "launch signal", Err: err}
	}

	waitCtx, cancel := context.WithTimeout(ctx, m.effectiveSuspendSignalTimeout())
	defer cancel()

	if err := launch.WaitForSavedSuspendState(waitCtx, manifest, m.effectiveSuspendSignalTimeout()); err != nil {
		return err
	}
	if err := launch.WaitForLaunchExited(waitCtx, manifest, m.effectiveSuspendSignalTimeout()); err != nil {
		return err
	}
	return nil
}

func (m *manager) launchPID() (int, error) {
	return launch.ResolveLaunchPID(m.launchManifest, m.effectivePIDSignaler())
}

func (m *manager) signalLaunchPID(pid int, sig os.Signal) error {
	if err := m.effectivePIDSignaler().Signal(pid, sig); err != nil {
		if launch.IsNoProcess(err) {
			return fmt.Errorf("stale launch pid %d: process does not exist", pid)
		}
		return fmt.Errorf("signal launch pid %d with %s: %w", pid, sig, err)
	}
	return nil
}

func (m *manager) effectivePIDSignaler() launch.PIDSignaler {
	if m.pidSignaler != nil {
		return m.pidSignaler
	}
	return syscallPIDSignaler{}
}

func (m *manager) effectiveSuspendSignalTimeout() time.Duration {
	shutdownDelay := m.shutdownDelay
	if shutdownDelay <= 0 {
		shutdownDelay = defaultShutdownDelay
	}

	teardownProcesses := 2 + len(m.launchManifest.Run) // qemu, active ssh session when present, plus run processes.

	return defaultLaunchSignalTimeout +
		m.effectiveQMPMigrationTimeout() +
		m.effectiveQMPQuitTimeout() +
		// The graceful-shutdown probe: a post-suspend guest is paused, so the
		// guest ping fails within its cap and teardown falls through to quit.
		m.effectiveQMPConnectTimeout() + guestShutdownResponseTimeout +
		time.Duration(teardownProcesses)*shutdownDelay
}
