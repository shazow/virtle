package manager

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/shazow/virtle/internal/balloon"
	"github.com/shazow/virtle/internal/hotplug"
	"github.com/shazow/virtle/internal/manager/launch"
	"github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/internal/qga"
)

// StartOptions configures a non-blocking StartVM.
type StartOptions struct {
	Resume ResumeMode // defaults to ResumeModeNo
}

// StartVM starts a VM from a resolved manifest and returns a handle without
// blocking on the session, unlike LaunchWithOptions. It is the seam the
// public backend/qemu package is built on: session-layer behavior (SSH
// readiness gating, foreground signal waits) stays out, while process
// supervision, QMP readiness, guest file writes, the control socket, and
// the balloon controller run as they do for CLI launches.
func StartVM(ctx context.Context, mf *manifest.Manifest, options StartOptions, configs ...Config) (*VM, error) {
	if options.Resume == "" {
		options.Resume = ResumeModeNo
	}
	config := DefaultConfig()
	if len(configs) > 0 {
		config = mergeConfig(config, configs[0])
	}
	m := newManagerFromConfig(config)
	plan, err := m.planLaunch(launch.Spec{Manifest: mf, Options: launch.Options{
		Resume:           options.Resume,
		SkipSSHReadyWait: true,
	}})
	if err != nil {
		return nil, err
	}
	running, err := m.startWithPlan(ctx, plan)
	if err != nil {
		return nil, err
	}
	if task := balloon.ControllerTask(running.qmp, plan.Manifest.QEMU.Devices.Balloon, plan.Notifier); task != nil {
		running.processes.StartTasks(running.ctx, task)
	}
	if plan.ResumeState != nil {
		if err := removeRestoredSuspendState(plan); err != nil {
			return nil, errors.Join(err, running.Close())
		}
	}
	return &VM{m: m, running: running}, nil
}

// VM is a running virtual machine started by StartVM. It stays alive until
// Close (graceful teardown), Kill (hard stop), or the VM exiting on its
// own; Wait reaps the exit and releases runtime state either way.
type VM struct {
	m       *manager
	running *runningLaunch

	closeOnce sync.Once
	closeErr  error
}

// Wait blocks until the VM exits (or ctx is done), then releases runtime
// state. A VM that exited by itself still needs its sockets, lock, and
// helper processes cleaned up, so Wait performs the teardown before
// returning.
func (v *VM) Wait(ctx context.Context) error {
	qemu := v.running.processes.QEMU()
	if qemu == nil {
		return errors.New("vm process is not running")
	}
	if err := qemu.WaitContext(ctx); err != nil {
		if ctx.Err() != nil {
			return err
		}
		// The VM exited with an error status; still tear down.
		return errors.Join(err, v.close())
	}
	return v.close()
}

// Kill hard-stops the VM immediately and releases runtime state, skipping
// the graceful guest shutdown path.
func (v *VM) Kill() error {
	var killErr error
	if qemu := v.running.processes.QEMU(); qemu != nil {
		killErr = qemu.KillAndWait()
	}
	return errors.Join(killErr, v.close())
}

// Close gracefully tears the VM down: guest shutdown when the guest agent
// answers, then QMP quit, then signals, then runtime cleanup. Safe to call
// more than once.
func (v *VM) Close() error {
	return v.close()
}

func (v *VM) close() error {
	v.closeOnce.Do(func() {
		v.closeErr = v.running.Close()
		v.m.writeLaunchStats(v.running.stats)
	})
	return v.closeErr
}

// CID returns the vsock CID allocated to the VM.
func (v *VM) CID() int { return v.running.plan.CID }

// StateDir returns the resolved persistence state directory.
func (v *VM) StateDir() string { return v.running.plan.Paths.StateDir }

// GuestAgentSocketPath returns the host path of the guest agent socket.
func (v *VM) GuestAgentSocketPath() string { return v.running.plan.Paths.GuestAgentSocket }

// DialGuestAgent waits for guest agent readiness and returns a connected
// client. The caller owns the client and must Disconnect it.
func (v *VM) DialGuestAgent(ctx context.Context) (qga.Client, error) {
	return v.m.waitForGuestAgent(ctx, v.running.plan.Paths.GuestAgentSocket, v.running.processes.Watchers())
}

// ShutdownGuest asks the guest to power down through the guest agent (or
// the manifest's shutdown_exec command). It does not wait for the VM to
// exit; pair it with Wait.
func (v *VM) ShutdownGuest(ctx context.Context) error {
	shutdown := v.running.plan.Manifest.QEMU.GuestAgent
	return v.m.requestGuestShutdown(ctx, v.running.plan.Paths.GuestAgentSocket, shutdown.ShutdownExec)
}

// Suspend saves the VM state to the manifest's state directory and tears
// the VM down, skipping guest file write-back exactly like a CLI suspend.
// The VM is not usable afterwards; resume with StartVM and
// ResumeModeForce.
func (v *VM) Suspend(ctx context.Context) error {
	plan := v.running.plan
	if err := v.m.saveSuspendStateConnected(ctx, plan.Paths.QMPSocket, v.running.qmp, plan.CID, plan.Notifier); err != nil {
		return err
	}
	v.running.runtime.MarkSavedSuspend()
	return v.close()
}

// ResizeMemory sets the virtio-balloon target for the running VM, in
// bytes. The manifest must configure a balloon device.
func (v *VM) ResizeMemory(ctx context.Context, sizeBytes int64) error {
	if v.running.plan.Manifest.QEMU.Devices.Balloon == nil {
		return fmt.Errorf("resize memory: no balloon device configured: %w", errors.ErrUnsupported)
	}
	return balloon.SetActual(ctx, v.running.qmp, sizeBytes)
}

// AttachHotplugDevice attaches an ad hoc hotplug device to the running VM.
// The VM must have been started with PCIe hotplug ports reserved (manifest
// [hotplug] devices reserve them).
func (v *VM) AttachHotplugDevice(ctx context.Context, dev hotplug.Device) error {
	runner := v.m.hotplugRunner(v.running.runtime.QMP())
	runner.Devices = append(append([]hotplug.Device(nil), runner.Devices...), dev)
	return runner.Attach(ctx, dev.ID)
}

// DetachHotplugDevice detaches a device previously attached with
// AttachHotplugDevice (or declared under manifest [hotplug]).
func (v *VM) DetachHotplugDevice(ctx context.Context, dev hotplug.Device) error {
	runner := v.m.hotplugRunner(v.running.runtime.QMP())
	runner.Devices = append(append([]hotplug.Device(nil), runner.Devices...), dev)
	return runner.Detach(ctx, dev.ID)
}
