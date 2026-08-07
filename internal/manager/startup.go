package manager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/shazow/virtle/internal/executor"
	controlpkg "github.com/shazow/virtle/internal/manager/control"
	"github.com/shazow/virtle/internal/manager/launch"
	runtimepkg "github.com/shazow/virtle/internal/manager/runtime"
	"github.com/shazow/virtle/internal/qmpclient"
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
		if err == nil {
			return
		}
		if started != nil {
			if launch.IsSavedSuspendExit(err) && started.runtime != nil {
				started.runtime.MarkSavedSuspend()
			}
			err = errors.Join(err, started.Close())
			m.writeLaunchStats(stats)
			return
		}

		var cleanupErr error
		cleanupErr = errors.Join(cleanupErr, processes.Close(m.shutdownDelay))
		cleanupErr = errors.Join(cleanupErr, cleanupRuntime())
		if qmp != nil {
			cleanupErr = errors.Join(cleanupErr, qmp.Disconnect())
		}
		if socketCleanupReached {
			cleanupErr = errors.Join(cleanupErr, launch.RemoveStaleSockets(plan.RuntimeSocketCleanupFiles()...))
		}
		m.writeLaunchStats(stats)
		err = errors.Join(err, cleanupErr)
	}()

	cid, err := launch.AcquireCID(plan.Manifest, plan.ResumeState, m.vsockCIDChecker)
	if err != nil {
		return nil, &launch.StageError{Stage: "preflight", Err: err}
	}
	qemuCmd, err := buildQEMUCommand(plan.Manifest, cid, plan.ResumeState != nil)
	if err != nil {
		return nil, &launch.StageError{Stage: "preflight", Err: err}
	}
	if qemuCmd == nil {
		return nil, &launch.StageError{Stage: "preflight", Err: errors.New("qemu command is required")}
	}
	plan.CID = cid
	plan.QEMUCommand = qemuCmd
	if err := m.prepareRuntimeState(plan); err != nil {
		return nil, &launch.StageError{Stage: "preflight", Err: err}
	}
	socketCleanupReached = true

	runProcesses, err := m.startRuns(plan.CID)
	if err != nil {
		return nil, err
	}
	processes.AddGroup(runProcesses)
	if len(plan.VirtioFSSocketPaths) > 0 {
		if err := m.waitForSockets(launchCtx, "virtiofs startup", plan.VirtioFSSocketPaths, processes.Watchers()); err != nil {
			return nil, err
		}
	}

	stats.Timer(launch.TimerBootStarted, time.Now())
	qemu, err := m.startQEMU(plan.QEMUCommand)
	if err != nil {
		return nil, launch.WrapFixedStage("vm startup")(err)
	}
	if qemu == nil {
		return nil, launch.WrapFixedStage("vm startup")(errors.New("qemu process is required"))
	}
	processes.SetQEMU(qemu)
	qmp, err = m.waitForQMP(launchCtx, plan.Paths.QMPSocket, processes.Watchers())
	if err != nil {
		return nil, err
	}
	if qmp == nil {
		return nil, launch.WrapFixedStage("vm startup")(errors.New("qmp client is required"))
	}
	stats.Timer(launch.TimerQMPReady, time.Now())
	qemu.SetShutdown(func() error {
		return qmp.Quit(m.effectiveQMPQuitTimeout())
	})

	if plan.ResumeState != nil {
		if err := m.restoreLaunchRuntime(launchCtx, plan, qmp); err != nil {
			return nil, err
		}
		writeBackOnExit = true
	}

	suspendHandler := newLaunchSuspendHandler(m, plan.Paths.QMPSocket, qmp, plan.CID, plan.Notifier, func() bool {
		return writeBackOnExit
	})
	runtime := runtimepkg.New(runtimepkg.RuntimeConfig{
		Manifest:        plan.Manifest,
		Paths:           plan.Paths,
		CID:             plan.CID,
		Stats:           stats,
		QMP:             qmp,
		SuspendRequests: lifecycle.Suspend(),
		Processes:       processes,
		ShutdownDelay:   m.shutdownDelay,
		WriteBack: func(ctx context.Context) error {
			if !writeBackOnExit {
				return nil
			}
			return m.writeBackGuestFiles(ctx, executor.Group{})
		},
		Cleanup: func() error {
			return errors.Join(launch.RemoveStaleSockets(plan.RuntimeSocketCleanupFiles()...), cleanupRuntime())
		},
		QMPTimeout:       m.effectiveQMPCommandTimeout(),
		Logger:           m.logger,
		SavedSuspendExit: launch.IsSavedSuspendExit,
	})
	started = &runningLaunch{
		ctx:            launchCtx,
		runtime:        runtime,
		plan:           plan,
		stats:          stats,
		qmp:            qmp,
		lifecycle:      lifecycle,
		suspendHandler: suspendHandler,
		processes:      processes,
	}
	runtime.SetReady()
	if _, err := runtime.StartControl(launchCtx, controlpkg.Handlers{
		Guest:   m.guestFeature(plan.Paths.GuestAgentSocket, processes),
		Hotplug: m.hotplugFeature(runtime.QMP()),
	}); err != nil {
		return nil, launch.WrapFixedStage("control startup")(err)
	}
	if err := launch.HandleQueuedSuspend(launchCtx, lifecycle, suspendHandler.Handle); err != nil {
		return nil, err
	}
	if plan.ResumeState == nil {
		if err := m.writeGuestFiles(launchCtx, stats, processes.Watchers()); err != nil {
			return nil, err
		}
		stats.Timer(launch.TimerFilesReady, time.Now())
		if plan.Paths.SSHReadySocket != "" {
			if m.logger != nil {
				m.logger.Info("waiting for ssh readiness")
			}
			if err := m.waitForSSHReady(launchCtx, plan.Paths.SSHReadySocket, processes.Watchers()); err != nil {
				return nil, err
			}
		}
		stats.Timer(launch.TimerSSHReady, time.Now())
		writeBackOnExit = true
	}
	return started, nil
}

func (m *manager) writeLaunchStats(stats *launch.Stats) {
	if stats == nil {
		return
	}
	stats.Timer(launch.TimerCompleted, time.Now())
	if output := m.outputWriter(); output != nil {
		fmt.Fprintf(output, "stats: %s\n", stats.String())
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
			m.logger.Info("restoring saved vsock cid", "cid", plan.CID)
		} else {
			m.logger.Info("allocated vsock cid", "cid", plan.CID)
		}
	}

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
	for _, path := range plan.ExternalVirtioFSSocketPaths {
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
	for _, path := range plan.VolumeImagePaths {
		dir := filepath.Dir(path)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create directory %q: %w", dir, err)
		}
	}
	if err := launch.RemoveStaleSockets(plan.RuntimeSocketCleanupFiles()...); err != nil {
		return err
	}
	for _, volume := range plan.Volumes {
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
		m.logger.Info("starting qemu")
	}
	return m.runner.Start(cmd)
}
