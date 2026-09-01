package vmm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/shazow/virtle/backend/qemu/internal/balloon"
	"github.com/shazow/virtle/backend/qemu/internal/launch"
	"github.com/shazow/virtle/backend/qemu/internal/qga"
	"github.com/shazow/virtle/internal/control"
	"github.com/shazow/virtle/internal/manifest"
)

// StartOptions configures a non-blocking StartVM.
type StartOptions struct {
	Resume ResumeMode // defaults to ResumeModeNo

	// HasRemoteControl declares whether the VM image runs a guest control
	// agent; see launch.Options.
	HasRemoteControl bool
}

// StartVM starts a VM from a resolved manifest and returns a handle without
// blocking on the session, unlike LaunchWithOptions. It is the seam the
// public backend/qemu package is built on: session-layer behavior (SSH
// readiness gating, foreground signal waits) stays out, while process
// supervision, QMP readiness, guest file writes, the control socket, and
// the balloon controller run as they do for CLI launches.
func StartVM(ctx context.Context, mf *manifest.Manifest, options StartOptions, configs ...Config) (*VM, error) {
	config := DefaultConfig()
	if len(configs) > 0 {
		config = mergeConfig(config, configs[0])
	}
	// Library starts install no process-global signal handlers — signal
	// policy belongs to the embedding program. A never-firing channel
	// keeps the launch lifecycle from claiming SIGINT/SIGTSTP/...;
	// Config.Signals may supply a real channel to opt in.
	if config.Signals == nil {
		config.Signals = make(chan os.Signal)
	}
	m := newManagerFromConfig(config)
	v, err := m.startVM(ctx, launch.Spec{Manifest: mf, Options: launch.Options{
		Resume:           options.Resume,
		HasRemoteControl: options.HasRemoteControl,
	}})
	if err != nil {
		return nil, err
	}
	// Library starts have no session phase, so a successful Start is the
	// point the restored suspend state stops being resumable. CLI sessions
	// remove it later, once the session is established.
	if v.running.plan.ResumeState != nil {
		if err := removeRestoredSuspendState(v.running.plan); err != nil {
			return nil, errors.Join(err, v.Close())
		}
	}
	return v, nil
}

// startVM plans and starts a launch, returning the VM handle. Both the
// library path (StartVM) and the CLI path (LaunchWithOptions) go through
// here; queued-suspend saves surface as launch.ErrSavedSuspendExit.
func (m *manager) startVM(ctx context.Context, spec launch.Spec) (*VM, error) {
	if spec.Options.Resume == "" {
		spec.Options.Resume = ResumeModeNo
	}
	plan, err := m.planLaunch(spec)
	if err != nil {
		return nil, err
	}
	running, err := m.startWithPlan(ctx, plan)
	if err != nil {
		return nil, err
	}
	return newVM(m, running), nil
}

// StartSessionVM starts a VM with CLI session semantics: the session
// options participate in preflight validation (an --ssh launch against a
// manifest with no ssh.exec fails before anything boots), and process
// signal handlers are installed unless Config.Signals overrides them. It
// is the bridge the CLI reaches through backend/qemu; pair it with
// RunSession.
func StartSessionVM(ctx context.Context, mf *manifest.Manifest, options StartOptions, session SessionOptions, configs ...Config) (*VM, error) {
	config := DefaultConfig()
	if len(configs) > 0 {
		config = mergeConfig(config, configs[0])
	}
	m := newManagerFromConfig(config)
	return m.startVM(ctx, launch.Spec{Manifest: mf, RemoteCommand: session.RemoteCommand, Options: launch.Options{
		Resume:           options.Resume,
		SSH:              session.SSH,
		HasRemoteControl: true, // CLI guests are expected to run qemu-guest-agent
	}})
}

// IsSavedSuspendExit reports whether err is the sentinel for a launch that
// ended by saving a suspend — a success for session purposes.
func IsSavedSuspendExit(err error) bool { return launch.IsSavedSuspendExit(err) }

// SessionOptions configures the CLI foreground session on a started VM.
type SessionOptions struct {
	SSH           bool      // attach the interactive SSH session loop
	RemoteCommand []string  // remote command for the SSH session
	Stdout        io.Writer // foreground session and SSH stdout; default: discard
	Stderr        io.Writer // foreground session and SSH stderr; default: discard
}

// RunSession runs the CLI foreground session on a started VM: the
// ssh-ready gate on fresh boots, then either the interactive SSH attach
// loop (with autoprovision and retries) or the connect hint plus the VM
// wait, with suspend-on-signal handling throughout. RunSession owns the
// VM: it is closed when the session ends, and a session that ends in a
// saved suspend reports success.
func RunSession(ctx context.Context, v *VM, opts SessionOptions) (err error) {
	m, running := v.m, v.running
	plan := running.plan
	if opts.SSH && len(plan.Manifest.SSH.Argv) == 0 {
		return errors.Join(fmt.Errorf("--ssh requires a non-empty manifest.ssh.exec"), v.Close())
	}
	plan.RemoteCommand = append([]string(nil), opts.RemoteCommand...)
	defer func() {
		joinDeferredError(&err, v.close)
	}()
	// The readiness gate is a session concern: the session blocks here so
	// the SSH hint (or the attach loop) lands on a reachable guest, while
	// bare StartVM callers return as soon as the VM is up.
	if plan.ResumeState == nil {
		if err := m.guestReadiness().WaitReady(running.ctx, plan, running.processes.Watchers()); err != nil {
			return err
		}
		m.sshLifecycleLogger().Info("guest is ready")
		running.stats.Timer(launch.TimerSSHReady, time.Now())
	}
	err = m.waitForRunningLaunch(ctx, running, opts)
	if launch.IsSavedSuspendExit(err) {
		return nil
	}
	return err
}

// VM is a running virtual machine started by StartVM. It stays alive until
// Close (graceful teardown), Kill (hard stop), or the VM exiting on its
// own; Wait reaps the exit and releases runtime state either way.
type VM struct {
	m       *manager
	running *runningLaunch

	closeOnce sync.Once
	closeErr  error
	done      chan struct{}
	exitErr   error
}

func newVM(m *manager, running *runningLaunch) *VM {
	v := &VM{m: m, running: running, done: make(chan struct{})}
	go v.reap()
	return v
}

func (v *VM) reap() {
	qemu := v.running.processes.QEMU()
	if qemu == nil {
		v.exitErr = errors.New("vm process is not running")
	} else {
		v.exitErr = qemu.Wait()
	}
	v.exitErr = errors.Join(v.exitErr, v.close())
	close(v.done)
}

// Done closes after the VM has exited and runtime state has been released.
func (v *VM) Done() <-chan struct{} { return v.done }

// Err returns the VM exit result after Done closes.
func (v *VM) Err() error { return v.exitErr }

// Wait blocks until the VM exits (or ctx is done), then releases runtime
// state. A VM that exited by itself still needs its sockets, lock, and
// helper processes cleaned up, so Wait performs the teardown before
// returning.
func (v *VM) Wait(ctx context.Context) error {
	select {
	case <-v.done:
		return v.exitErr
	case <-ctx.Done():
		return context.Cause(ctx)
	}
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

// Close gracefully tears the VM down: guest shutdown, then QMP quit, then
// signals, then runtime cleanup. The graceful guest shutdown is attempted
// only when the VM has remote control; without it teardown goes straight
// to QMP quit. Safe to call more than once.
func (v *VM) Close() error {
	return v.close()
}

// Shutdown gracefully tears the VM down within ctx, escalating process
// shutdown to a hard kill when the context expires.
func (v *VM) Shutdown(ctx context.Context) error {
	return v.closeContext(ctx)
}

func (v *VM) close() error {
	return v.closeContext(context.Background())
}

func (v *VM) closeContext(ctx context.Context) error {
	v.closeOnce.Do(func() {
		v.closeErr = v.running.runtime.Shutdown(ctx)
		v.m.writeLaunchStats(v.running.stats)
	})
	return v.closeErr
}

// StateDir returns the resolved persistence state directory.
func (v *VM) StateDir() string { return v.running.plan.Paths.StateDir }

// Status reports the in-process runtime status using the control-socket shape.
func (v *VM) Status(ctx context.Context) (control.StatusResponse, error) {
	return v.running.runtime.Status(ctx, control.StatusRequest{})
}

// DialGuestAgent waits for guest agent readiness and returns a connected
// client. The caller owns the client and must Disconnect it.
func (v *VM) DialGuestAgent(ctx context.Context) (qga.Client, error) {
	if !v.running.plan.Options.HasRemoteControl {
		return nil, fmt.Errorf("guest agent: vm has no remote control: %w", errors.ErrUnsupported)
	}
	return v.m.waitForGuestAgent(ctx, v.running.plan.Paths.GuestAgentSocket, v.running.processes.Watchers())
}

// ShutdownGuest asks the guest to power down through the guest agent (or
// the manifest's shutdown_exec command). It does not wait for the VM to
// exit; pair it with Wait.
func (v *VM) ShutdownGuest(ctx context.Context) error {
	if !v.running.plan.Options.HasRemoteControl {
		return fmt.Errorf("guest shutdown: vm has no remote control: %w", errors.ErrUnsupported)
	}
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

func (v *VM) ResolveHotplugMount(entry manifest.MountEntry) (manifest.HotplugDevice, error) {
	return v.running.plan.Manifest.ResolveHotplugMount(entry)
}

func (v *VM) ResolveHotplugNetwork(entry manifest.NetworkInput) (manifest.HotplugDevice, error) {
	return v.running.plan.Manifest.ResolveHotplugNetwork(entry)
}

// AttachHotplugDevice attaches an ad hoc hotplug device to the running VM.
// The VM must have been started with PCIe hotplug ports reserved (manifest
// [hotplug] devices reserve them).
func (v *VM) AttachHotplugDevice(ctx context.Context, dev manifest.HotplugDevice) error {
	runner := v.m.hotplugRunner(v.running.runtime.QMP())
	return runner.AttachDevice(ctx, dev)
}

// DetachHotplugDevice detaches a device previously attached with
// AttachHotplugDevice (or declared under manifest [hotplug]).
func (v *VM) DetachHotplugDevice(ctx context.Context, id string) error {
	runner := v.m.hotplugRunner(v.running.runtime.QMP())
	return runner.Detach(ctx, id)
}
