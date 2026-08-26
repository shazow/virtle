package vmm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	shellquote "github.com/kballard/go-shellquote"
	"github.com/shazow/virtle/backend/qemu/internal/balloon"
	"github.com/shazow/virtle/backend/qemu/internal/launch"
	"github.com/shazow/virtle/backend/qemu/internal/qmpclient"
	"github.com/shazow/virtle/backend/qemu/internal/qmpwire"
	runtimepkg "github.com/shazow/virtle/backend/qemu/internal/runtime"
	controlpkg "github.com/shazow/virtle/internal/control"
	"github.com/shazow/virtle/internal/executor"
	"github.com/shazow/virtle/internal/manifest"
)

func (m *manager) startWithPlan(ctx context.Context, plan *launch.Plan) (started *runningLaunch, err error) {
	if plan == nil {
		return nil, &launch.StageError{Stage: "preflight", Err: errors.New("launch plan is required")}
	}
	m.launchManifest = plan.Manifest

	stats := launch.NewStats(time.Now())
	launchCtx, cancelLaunch := context.WithCancel(ctx)
	lifecycle := launch.NewSignalLifecycle(m.signals, cancelLaunch)
	runtimeLock, err := launch.AcquireRuntimeLock(launch.RuntimeLockSpec{
		Manifest:    plan.Manifest,
		ResumeState: plan.ResumeState,
		Locker:      m.locker,
		Lifecycle:   lifecycle,
		Cancel:      cancelLaunch,
		PID:         os.Getpid(),
	})
	if err != nil {
		return nil, &launch.StageError{Stage: "preflight", Err: err}
	}
	if runtimeLock == nil {
		stopLaunchLifecycle(lifecycle, cancelLaunch)
		return nil, &launch.StageError{Stage: "preflight", Err: errors.New("runtime lock is required")}
	}

	processes := launch.NewProcessSet()
	var qmp qmpclient.Client
	writeBackOnExit := false
	socketCleanupReached := false
	cleanupRuntime := func() error { return runtimeLock.Cleanup() }
	defer func() {
		err = m.finishFailedLaunch(err, started, processes, qmp, socketCleanupReached, cleanupRuntime, stats, plan)
	}()

	if err := m.prepareLaunchPreflight(plan); err != nil {
		return nil, err
	}
	socketCleanupReached = true

	if err := m.startLaunchProcesses(launchCtx, plan, processes); err != nil {
		return nil, err
	}

	qmp, err = m.launchQEMUWithQMP(launchCtx, plan, processes, stats)
	if err != nil {
		return nil, err
	}

	if plan.ResumeState != nil {
		if err := m.restoreLaunchRuntime(launchCtx, plan, qmp); err != nil {
			return nil, err
		}
		writeBackOnExit = true
	}

	started = m.newRunningLaunch(launchCtx, plan, stats, qmp, lifecycle, processes, cleanupRuntime, &writeBackOnExit)
	if err := m.completeLaunchStartup(started, &writeBackOnExit); err != nil {
		return nil, err
	}
	return started, nil
}

func (m *manager) finishFailedLaunch(err error, started *runningLaunch, processes *launch.ProcessSet, qmp qmpclient.Client, socketCleanupReached bool, cleanupRuntime func() error, stats *launch.Stats, plan *launch.Plan) error {
	if err == nil {
		return nil
	}
	if started != nil {
		if launch.IsSavedSuspendExit(err) && started.runtime != nil {
			started.runtime.MarkSavedSuspend()
		}
		err = errors.Join(err, started.Close())
		m.writeLaunchStats(stats)
		return err
	}

	var cleanupErr error
	cleanupErr = errors.Join(cleanupErr, processes.Close(context.Background()))
	cleanupErr = errors.Join(cleanupErr, cleanupRuntime())
	if qmp != nil {
		cleanupErr = errors.Join(cleanupErr, qmp.Disconnect())
	}
	if socketCleanupReached {
		cleanupErr = errors.Join(cleanupErr, launch.RemoveStaleSockets(plan.RuntimeSocketCleanupFiles()...))
	}
	m.writeLaunchStats(stats)
	return errors.Join(err, cleanupErr)
}

func (m *manager) prepareLaunchPreflight(plan *launch.Plan) error {
	cid, err := launch.AcquireCID(plan.Manifest, plan.ResumeState, m.vsockCIDChecker)
	if err != nil {
		return &launch.StageError{Stage: "preflight", Err: err}
	}
	qemuCmd, err := buildQEMUCommand(plan.Manifest, cid, plan.ResumeState != nil, m.consoleOutput)
	if err != nil {
		return &launch.StageError{Stage: "preflight", Err: err}
	}
	if qemuCmd == nil {
		return &launch.StageError{Stage: "preflight", Err: errors.New("qemu command is required")}
	}
	plan.CID = cid
	plan.QEMUCommand = qemuCmd
	if err := m.prepareRuntimeState(plan); err != nil {
		return &launch.StageError{Stage: "preflight", Err: err}
	}
	return nil
}

func (m *manager) startLaunchProcesses(launchCtx context.Context, plan *launch.Plan, processes *launch.ProcessSet) error {
	runProcesses, err := m.startRuns(plan.CID)
	if err != nil {
		return err
	}
	processes.AddGroup(runProcesses)
	if len(plan.VirtioFSSocketPaths) > 0 {
		if err := m.waitForSockets(launchCtx, "virtiofs startup", plan.VirtioFSSocketPaths, processes.Watchers()); err != nil {
			return err
		}
	}
	return nil
}

func (m *manager) launchQEMUWithQMP(launchCtx context.Context, plan *launch.Plan, processes *launch.ProcessSet, stats *launch.Stats) (qmpclient.Client, error) {
	stats.Timer(launch.TimerBootStarted, time.Now())
	qemu, err := m.startQEMU(plan.QEMUCommand)
	if err != nil {
		return nil, launch.WrapFixedStage("vm startup")(err)
	}
	if qemu == nil {
		return nil, launch.WrapFixedStage("vm startup")(errors.New("qemu process is required"))
	}
	processes.SetQEMU(qemu)
	qmp, err := m.waitForQMP(launchCtx, plan.Paths.QMPSocket, processes.Watchers())
	if err != nil {
		return qmp, err
	}
	if qmp == nil {
		return nil, launch.WrapFixedStage("vm startup")(errors.New("qmp client is required"))
	}
	stats.Timer(launch.TimerQMPReady, time.Now())
	qemu.SetShutdown(m.launchShutdownFunc(plan, qemu, qmp))
	return qmp, nil
}

func (m *manager) launchShutdownFunc(plan *launch.Plan, qemu *executor.Process, qmp qmpclient.Client) func() error {
	return func() error {
		// Shutdown runs during teardown, after the launch context may already
		// be canceled, so each step gets its own context. The graceful guest
		// shutdown is attempted only when the VM has remote control; without
		// it there is nobody in the guest to ask, so teardown goes straight
		// to QMP quit.
		if plan.Options.HasRemoteControl {
			shutdown := plan.Manifest.QEMU.GuestAgent
			method := "guest-shutdown"
			if len(shutdown.ShutdownExec) > 0 {
				method = "guest-exec"
			}
			m.logger.Info("requesting guest shutdown", "method", method, "exec", shutdown.ShutdownExec)
			err := m.requestGuestShutdown(context.Background(), plan.Paths.GuestAgentSocket, shutdown.ShutdownExec)
			if err != nil {
				m.logger.Warn("guest shutdown request failed; forcing qemu quit", "err", err)
			} else {
				m.logger.Info("waiting for guest shutdown", "timeout", shutdown.ShutdownTimeout)
				waitCtx, cancel := context.WithTimeoutCause(context.Background(), shutdown.ShutdownTimeout,
					fmt.Errorf("guest did not exit within %s", shutdown.ShutdownTimeout))
				waitErr := qemu.WaitContext(waitCtx)
				cancel()
				if waitErr == nil {
					m.logger.Info("guest shutdown completed")
					return nil
				}
				m.logger.Warn("guest shutdown timed out; forcing qemu quit", "err", waitErr)
			}
		} else {
			m.logger.Info("vm has no remote control; skipping guest shutdown request")
		}
		m.logger.Info("forcing qemu quit through QMP")
		ctx, cancel := context.WithTimeout(context.Background(), m.effectiveQMPQuitTimeout())
		defer cancel()
		quitErr := qmp.Quit(ctx)
		if errors.Is(quitErr, qmpwire.ErrBroken) {
			// The long-lived monitor was poisoned by an earlier interrupted
			// operation; retry on a fresh connection so teardown still quits
			// through QMP instead of falling to signals.
			quitErr = m.quitFreshQMP(ctx, plan.Paths.QMPSocket)
		}
		return quitErr
	}
}

func (m *manager) newRunningLaunch(launchCtx context.Context, plan *launch.Plan, stats *launch.Stats, qmp qmpclient.Client, lifecycle *launch.Lifecycle, processes *launch.ProcessSet, cleanupRuntime func() error, writeBackOnExit *bool) *runningLaunch {
	suspendHandler := newLaunchSuspendHandler(m, plan.Paths.QMPSocket, qmp, plan.CID, plan.Notifier, func() bool {
		return *writeBackOnExit
	})
	runtime := runtimepkg.New(runtimepkg.RuntimeConfig{
		Manifest:        plan.Manifest,
		Paths:           plan.Paths,
		CID:             plan.CID,
		Stats:           stats,
		QMP:             qmp,
		SuspendRequests: lifecycle.Suspend(),
		Processes:       processes,
		WriteBack: func(ctx context.Context) error {
			if !*writeBackOnExit {
				return nil
			}
			return m.writeBackGuestFiles(ctx, executor.Group{})
		},
		Cleanup: func() error {
			return errors.Join(launch.RemoveStaleSockets(plan.RuntimeSocketCleanupFiles()...), cleanupRuntime())
		},
		WriteBackTimeout: defaultWriteBackTimeout,
		Logger:           m.logger,
		SavedSuspendExit: launch.IsSavedSuspendExit,
	})
	return &runningLaunch{
		ctx:            launchCtx,
		runtime:        runtime,
		plan:           plan,
		stats:          stats,
		qmp:            qmp,
		lifecycle:      lifecycle,
		suspendHandler: suspendHandler,
		processes:      processes,
	}
}

func (m *manager) completeLaunchStartup(started *runningLaunch, writeBackOnExit *bool) error {
	launchCtx := started.ctx
	plan := started.plan
	started.runtime.SetReady()
	if _, err := started.runtime.StartControl(launchCtx, controlpkg.Handlers{
		Guest:   m.guestFeature(plan.Paths.GuestAgentSocket, started.processes),
		Hotplug: m.hotplugFeature(started.runtime.QMP()),
	}); err != nil {
		return launch.WrapFixedStage("control startup")(err)
	}
	if err := launch.HandleQueuedSuspend(launchCtx, started.lifecycle, started.suspendHandler.Handle); err != nil {
		return err
	}
	if plan.ResumeState == nil {
		if err := m.writeInitialGuestFiles(launchCtx, plan, started.stats, started.processes); err != nil {
			return err
		}
		*writeBackOnExit = plan.Options.HasRemoteControl
	}
	if task := balloon.ControllerTask(started.qmp, plan.Manifest.QEMU.Devices.Balloon, plan.Notifier, m.balloonLogger); task != nil {
		started.processes.StartTasks(launchCtx, task)
	}
	return nil
}

func (m *manager) writeInitialGuestFiles(launchCtx context.Context, plan *launch.Plan, stats *launch.Stats, processes *launch.ProcessSet) error {
	if plan.Options.HasRemoteControl {
		if err := m.writeGuestFiles(launchCtx, stats, processes.Watchers()); err != nil {
			return err
		}
	} else if len(plan.Manifest.ResolvedWriteFiles()) > 0 || plan.Manifest.Workspace.MountCWD {
		return &launch.StageError{Stage: "guest agent", Err: errors.New("guest file writes require remote control (write_files, workspace.mount_cwd)")}
	}
	stats.Timer(launch.TimerFilesReady, time.Now())
	return nil
}

func (m *manager) writeLaunchStats(stats *launch.Stats) {
	if stats == nil {
		return
	}
	stats.Timer(launch.TimerCompleted, time.Now())
	if m.logger != nil {
		m.logger.Debug("launch stats", "stats", stats.String())
	}
}

func stopLaunchLifecycle(lifecycle *launch.Lifecycle, cancel context.CancelFunc) {
	if lifecycle != nil {
		lifecycle.Stop()
	}
	if cancel != nil {
		cancel()
	}
}

func (m *manager) prepareRuntimeState(plan *launch.Plan) error {
	if m.logger != nil {
		if plan.ResumeState != nil {
			m.logger.Debug("restoring saved vsock cid", "cid", plan.CID)
		} else {
			m.logger.Debug("allocated vsock cid", "cid", plan.CID)
		}
	}

	if err := createRuntimeDirectories(plan); err != nil {
		return err
	}
	if err := checkExternalVirtioFSSockets(plan.ExternalVirtioFSSocketPaths); err != nil {
		return err
	}
	for _, path := range plan.VolumeImagePaths {
		dir := filepath.Dir(path)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create directory %q: %w", dir, err)
		}
	}
	if err := launch.RemoveStaleSockets(plan.RuntimeSocketCleanupFiles()...); err != nil {
		return err
	}
	return m.createVolumeImages(plan.Volumes)
}

func createRuntimeDirectories(plan *launch.Plan) error {
	for _, dir := range plan.Manifest.ResolvedPersistenceDirectories() {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create directory %q: %w", dir, err)
		}
	}
	for _, path := range plan.RuntimeSocketCleanupFiles() {
		dir := filepath.Dir(path)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create directory %q: %w", dir, err)
		}
	}
	return nil
}

func checkExternalVirtioFSSockets(paths []string) error {
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("external virtiofs socket %q does not exist", path)
			}
			return fmt.Errorf("stat external virtiofs socket %q: %w", path, err)
		}
		if info.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("external virtiofs socket %q is not a socket", path)
		}
	}
	return nil
}

func (m *manager) createVolumeImages(volumes []manifest.Volume) error {
	for _, volume := range volumes {
		if !volume.AutoCreate {
			continue
		}
		info, err := os.Stat(volume.ImagePath)
		if err == nil {
			if info.IsDir() {
				return fmt.Errorf("volume image %q is a directory", volume.ImagePath)
			}
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("stat volume image %q: %w", volume.ImagePath, err)
		}
		if m.logger != nil {
			m.logger.Info("creating volume image", "path", volume.ImagePath, "size_mib", volume.Size, "fs_type", volume.FSType)
		}
		if err := launch.CreateVolumeImage(volume); err != nil {
			return err
		}
	}
	return nil
}

func (m *manager) startQEMU(cmd *exec.Cmd) (*executor.Process, error) {
	if m.runner == nil {
		return nil, fmt.Errorf("qemu runner is not configured")
	}
	if m.logger != nil {
		m.logger.Debug("starting qemu", "command", shellquote.Join(cmd.Args...))
	}
	return m.startManagedProcess(cmd)
}
