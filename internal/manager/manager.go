// Package manager runs the host-side sandbox launcher lifecycle.
//
// It takes a validated launch manifest, prepares runtime directories and
// volume images, starts the supporting host processes, waits for QMP readiness,
// and then either hands control to the interactive SSH session or keeps the VM
// lifecycle in the foreground for out-of-band SSH. Teardown also
// lives here: balloon controller tasks stop first, then any active session and
// helper daemons are shut down, and QEMU is asked to exit through QMP before
// any forced process cleanup is used.
package manager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/shazow/virtle/internal/balloon"
	"github.com/shazow/virtle/internal/executor"
	"github.com/shazow/virtle/internal/manager/launch"
	"github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/internal/qga"
	"github.com/shazow/virtle/internal/qmpclient"
	"github.com/shazow/virtle/internal/sshtools"
)

const (
	defaultSSHRetryDelay      = 500 * time.Millisecond
	defaultShutdownDelay      = 15 * time.Second
	defaultGuestExecPollDelay = 100 * time.Millisecond
	sshRetryOutputRevealDelay = 250 * time.Millisecond
)

type manager struct {
	// launchManifest is bound once where each operation enters (startWithPlan
	// for launches, the Suspend and Hotplug wrappers otherwise); one manager
	// serves exactly one manifest.
	launchManifest *manifest.Manifest

	locker              launch.Locker
	vsockCIDChecker     launch.VSockCIDChecker
	runner              launch.Runner
	socketWaiter        launch.SocketWaiter
	qmpDialer           qmpclient.Dialer
	guestAgentDialer    qga.Dialer
	sshReadyDialer      launch.SSHReadyDialer
	logger              *slog.Logger
	logWriter           io.Writer
	sshRetryDelay       time.Duration
	sshReadyTimeout     time.Duration
	shutdownDelay       time.Duration
	qmpRetryDelay       time.Duration
	qmpConnectTimeout   time.Duration
	qmpQuitTimeout      time.Duration
	qmpMigrationTimeout time.Duration
	signals             <-chan os.Signal
	pidSignaler         launch.PIDSignaler
	notifier            launch.NotificationSink
}

func newManager() *manager {
	return newManagerFromConfig(DefaultConfig())
}

func newManagerFromConfig(config Config) *manager {
	config = mergeConfig(DefaultConfig(), config)
	return &manager{
		locker:              config.Locker,
		vsockCIDChecker:     config.VSockCIDChecker,
		runner:              config.Runner,
		socketWaiter:        config.SocketWaiter,
		qmpDialer:           config.QMPDialer,
		guestAgentDialer:    config.GuestAgentDialer,
		sshReadyDialer:      config.SSHReadyDialer,
		logger:              config.Logger,
		logWriter:           config.LogWriter,
		sshRetryDelay:       config.SSHRetryDelay,
		sshReadyTimeout:     config.SSHReadyTimeout,
		shutdownDelay:       config.ShutdownDelay,
		qmpRetryDelay:       config.QMPRetryDelay,
		qmpConnectTimeout:   config.QMPConnectTimeout,
		qmpQuitTimeout:      config.QMPQuitTimeout,
		qmpMigrationTimeout: config.QMPMigrationTimeout,
		signals:             config.Signals,
		pidSignaler:         config.PIDSignaler,
		notifier:            config.Notifier,
	}
}

func (m *manager) launch(ctx context.Context, manifest *manifest.Manifest, remoteCommand []string) error {
	return m.launchWithOptions(ctx, manifest, remoteCommand, launch.Options{Resume: launch.ResumeModeNo, SSH: true})
}

func (m *manager) launchWithOptions(ctx context.Context, manifest *manifest.Manifest, remoteCommand []string, options launch.Options) error {
	plan, err := m.planLaunch(launch.Spec{Manifest: manifest, RemoteCommand: remoteCommand, Options: options})
	if err != nil {
		return err
	}
	return m.launchWithPlan(ctx, plan)
}

func (m *manager) planLaunch(spec launch.Spec) (*launch.Plan, error) {
	cfg := spec.Manifest
	options := spec.Options
	resumeMode, err := launch.NormalizeResumeMode(options.Resume)
	if err != nil {
		return nil, &launch.StageError{Stage: "preflight", Err: err}
	}
	resumeState, err := launch.ResolveResumeState(cfg, resumeMode)
	if err != nil {
		return nil, &launch.StageError{Stage: "restore", Err: err}
	}
	notifier := m.notifier
	if notifier == nil {
		notifier = newCommandNotifier(cfg, m.logger)
	}
	plan, err := launch.BuildPlan(spec, resumeState, notifier)
	if err != nil {
		return nil, &launch.StageError{Stage: "preflight", Err: err}
	}
	return plan, nil
}

func (m *manager) launchWithPlan(ctx context.Context, plan *launch.Plan) (err error) {
	running, err := m.startWithPlan(ctx, plan)
	if err != nil {
		if launch.IsSavedSuspendExit(err) {
			return nil
		}
		return err
	}
	defer func() {
		joinDeferredError(&err, running.Close)
		m.writeLaunchStats(running.stats)
	}()
	err = m.waitForRunningLaunch(ctx, running, plan.Options.WaitMode())
	if launch.IsSavedSuspendExit(err) {
		return nil
	}
	return err
}

func (m *manager) restoreLaunchRuntime(ctx context.Context, plan *launch.Plan, client qmpclient.Client) error {
	if plan == nil || plan.ResumeState == nil {
		return fmt.Errorf("restore plan is not configured")
	}
	if m.logger != nil {
		m.logger.Info("restoring vm state", "path", plan.ResumeState.VMStatePath)
	}
	migrateCtx, cancel := m.migrationContext(ctx)
	defer cancel()
	if err := qmpclient.RestoreFromFile(migrateCtx, client, plan.ResumeState.VMStatePath); err != nil {
		return launch.WrapFixedStage("restore")(err)
	}
	notifyRuntimeResume(ctx, plan)
	return nil
}

func removeRestoredSuspendState(plan *launch.Plan) error {
	if err := launch.RemoveRestoredSuspendState(plan); err != nil {
		return &launch.StageError{Stage: "restore", Err: err}
	}
	return nil
}

func (m *manager) waitForLaunchForeground(
	ctx context.Context,
	plan *launch.Plan,
	stats *launch.Stats,
	qmpClient qmpclient.Client,
	lifecycle *launch.Lifecycle,
	suspendHandler suspendHandler,
	processes *launch.ProcessSet,
) error {
	if task := balloon.ControllerTask(qmpClient, plan.Manifest.QEMU.Devices.Balloon, plan.Notifier); task != nil {
		processes.StartTasks(ctx, task)
	}

	if plan.Options.SSH && len(plan.Manifest.SSH.Argv) > 0 {
		if err := m.runSSHSession(ctx, plan, stats, lifecycle, suspendHandler, processes); err != nil {
			return err
		}
		if plan.ResumeState != nil {
			return removeRestoredSuspendState(plan)
		}
		return nil
	}

	if plan.ResumeState != nil {
		if err := removeRestoredSuspendState(plan); err != nil {
			return err
		}
	}

	renderer, err := manifest.NewTemplateRenderer(manifest.SSHTemplateProvider{
		CID:         plan.CID,
		User:        plan.Manifest.SSH.User,
		Destination: sshtools.VSockDestination(plan.Manifest.SSH.User, plan.CID),
	})
	if err != nil {
		if m.logger != nil {
			m.logger.Info("ssh command hint template failed", "err", err)
		}
	} else {
		argv, err := renderer.RenderArgv(plan.Manifest.SSH.Argv)
		if err != nil {
			if m.logger != nil {
				m.logger.Info("ssh command hint template failed", "err", err)
			}
		} else if hint := (sshtools.Config{Exec: argv, User: plan.Manifest.SSH.User}).Hint(plan.CID); hint != "" {
			fmt.Fprintf(m.outputWriter(), "connect with: %s\n", hint)
		}
	}

	vmWatchers := processes.VMWatchers()
	return m.waitForVM(ctx, processes.QEMU(), lifecycle, suspendHandler, plan.Paths.GuestAgentSocket, vmWatchers)
}

func (m *manager) startManagedProcess(cmd *exec.Cmd) (*executor.Process, error) {
	return m.runner.Start(cmd)
}

func (m *manager) startRuns(cid int) (executor.Group, error) {
	manifest := m.launchManifest
	runs, err := manifest.ResolvedRuns(cid)
	if err != nil {
		return executor.Group{}, &launch.StageError{Stage: "run startup", Err: err}
	}
	if len(runs) == 0 {
		return executor.NewGroup(), nil
	}
	if m.runner == nil {
		return executor.Group{}, &launch.StageError{Stage: "run startup", Err: fmt.Errorf("run starter is not configured")}
	}

	started := executor.NewGroup()
	for i, run := range runs {
		if m.logger != nil {
			m.logger.Info("starting run", "index", i)
		}
		cmd := executor.Command(run.Exec[0], run.Exec[1:], run.Env)
		cmd.Dir = run.Dir
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		process, err := m.runner.Start(cmd)
		if err != nil {
			_ = started.StopAll(m.shutdownDelay)
			return executor.Group{}, &launch.StageError{Stage: "run startup", Err: err}
		}
		started.Add(process)
	}

	return started, nil
}

func (m *manager) waitForSockets(ctx context.Context, stage string, socketPaths []string, watchers executor.Group) error {
	return m.waitForLaunchSockets(ctx, stage, socketPaths, watchers)
}

func (m *manager) waitForQMP(ctx context.Context, socketPath string, watchers executor.Group) (qmpclient.Client, error) {
	dialer := m.qmpDialer
	if dialer == nil {
		dialer = &qmpclient.SocketMonitorDialer{}
	}
	retryDelay := m.qmpRetryDelay
	if retryDelay <= 0 {
		retryDelay = defaultQMPRetryDelay
	}
	return launch.WaitForQMP(ctx, launch.QMPWait{
		Stage:          "vm startup",
		SocketPath:     socketPath,
		SocketWaiter:   m.socketWaiter,
		Dialer:         dialer,
		ConnectTimeout: m.effectiveQMPConnectTimeout(),
		RetryDelay:     retryDelay,
		PollDelay:      defaultSocketPollInterval,
		Watchers:       watchers,
	})
}

func (m *manager) waitForLaunchSockets(ctx context.Context, stage string, socketPaths []string, watchers executor.Group) error {
	return launch.WaitForSockets(ctx, launch.SocketWait{
		Stage:        stage,
		SocketPaths:  socketPaths,
		SocketWaiter: m.socketWaiter,
		PollDelay:    defaultSocketPollInterval,
		Watchers:     watchers,
	})
}

func (m *manager) effectiveQMPConnectTimeout() time.Duration {
	if m.qmpConnectTimeout > 0 {
		return m.qmpConnectTimeout
	}
	return defaultQMPConnectTimeout
}

func (m *manager) effectiveQMPQuitTimeout() time.Duration {
	if m.qmpQuitTimeout > 0 {
		return m.qmpQuitTimeout
	}
	return defaultQMPQuitTimeout
}

func (m *manager) effectiveQMPMigrationTimeout() time.Duration {
	if m.qmpMigrationTimeout > 0 {
		return m.qmpMigrationTimeout
	}
	return defaultQMPMigrationTimeout
}

// migrationContext bounds a QMP migration with the configured migration
// timeout.
func (m *manager) migrationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := m.effectiveQMPMigrationTimeout()
	return context.WithTimeoutCause(ctx, timeout, fmt.Errorf("migration timed out after %s", timeout))
}

func (m *manager) effectiveQMPCommandTimeout() time.Duration {
	return m.effectiveQMPConnectTimeout()
}

type launchSuspendHandler struct {
	manager       *manager
	qmpSocketPath string
	client        qmpclient.Client
	cid           int
	notifier      launch.NotificationSink
	writeBack     func() bool
	once          sync.Once
	err           error
}

type suspendHandler interface {
	Handle(context.Context, *launch.SuspendCoordinator) error
}

func newLaunchSuspendHandler(manager *manager, qmpSocketPath string, client qmpclient.Client, cid int, notifier launch.NotificationSink, writeBack func() bool) *launchSuspendHandler {
	return &launchSuspendHandler{
		manager:       manager,
		qmpSocketPath: qmpSocketPath,
		client:        client,
		cid:           cid,
		notifier:      notifier,
		writeBack:     writeBack,
	}
}

func handleSuspendRequest(ctx context.Context, coordinator *launch.SuspendCoordinator, handler *launchSuspendHandler) error {
	coordinator.Begin()
	err := handler.saveAndExit(ctx)
	coordinator.Complete(err)
	return err
}

func (h *launchSuspendHandler) Handle(ctx context.Context, coordinator *launch.SuspendCoordinator) error {
	return handleSuspendRequest(ctx, coordinator, h)
}

func (h *launchSuspendHandler) saveAndExit(ctx context.Context) error {
	h.once.Do(func() {
		if h.writeBack != nil && h.writeBack() {
			if err := h.manager.writeBackGuestFiles(ctx, executor.Group{}); err != nil {
				h.err = err
				return
			}
		}
		if err := h.manager.saveSuspendStateConnected(ctx, h.qmpSocketPath, h.client, h.cid, h.notifier); err != nil {
			h.err = err
			return
		}
		h.err = launch.ErrSavedSuspendExit
	})
	return h.err
}

func (m *manager) runSSHSession(
	ctx context.Context,
	plan *launch.Plan,
	stats *launch.Stats,
	lifecycle *launch.Lifecycle,
	suspendHandler suspendHandler,
	processes *launch.ProcessSet,
) error {
	return launch.RunSSHSession(ctx, launch.SSHSession{
		Plan:                   plan,
		Runner:                 m.runner,
		Logger:                 m.logger,
		Output:                 m.outputWriter(),
		RetryOutputRevealDelay: sshRetryOutputRevealDelay,
		AddProcesses:           processes.Add,
		RemoveProcess:          processes.Remove,
		Watchers:               processes.Watchers,
		RecordTimer:            stats.Timer,
		Wait: func(ctx context.Context, session *executor.Process, watchers executor.Group) error {
			return m.waitForSession(ctx, session, lifecycle, suspendHandler, plan.Paths.GuestAgentSocket, watchers)
		},
		WaitForRetry: func(ctx context.Context, watchers executor.Group) error {
			return m.waitBeforeSSHRetry(ctx, lifecycle, suspendHandler, plan.Paths.GuestAgentSocket, watchers)
		},
		EnsureKey:  m.ensureSSHAutoprovisionKey,
		InstallKey: m.installSSHAutoprovisionKey,
	})
}

func (m *manager) waitBeforeSSHRetry(ctx context.Context, lifecycle *launch.Lifecycle, suspendHandler suspendHandler, guestAgentSocketPath string, watchers executor.Group) error {
	// Validation rejects non-positive retry delays; the manager default applies
	// only when no manifest is bound.
	delay := m.launchManifest.SSHRetryDelay(m.sshRetryDelay)
	if delay <= 0 {
		return nil
	}

	return m.waitForLifecycleEvent(ctx, "active session", delay, lifecycle, suspendHandler, guestAgentSocketPath, watchers)
}

func (m *manager) waitForSession(ctx context.Context, session *executor.Process, lifecycle *launch.Lifecycle, suspendHandler suspendHandler, guestAgentSocketPath string, watchers executor.Group) error {
	return m.waitForProcess(ctx, "active session", session, 0, lifecycle, suspendHandler, guestAgentSocketPath, watchers)
}

func (m *manager) waitForVM(ctx context.Context, qemu *executor.Process, lifecycle *launch.Lifecycle, suspendHandler suspendHandler, guestAgentSocketPath string, watchers executor.Group) error {
	return m.waitForProcess(ctx, "vm session", qemu, 0, lifecycle, suspendHandler, guestAgentSocketPath, watchers)
}

func (m *manager) waitForProcess(ctx context.Context, stage string, process *executor.Process, delay time.Duration, lifecycle *launch.Lifecycle, suspendHandler suspendHandler, guestAgentSocketPath string, watchers executor.Group) error {
	wait := launch.LifecycleProcessWait{
		Stage:     stage,
		Delay:     delay,
		Lifecycle: lifecycle,
		Watchers:  watchers,
		PollDelay: defaultSocketPollInterval,
		Suspend: func(ctx context.Context) error {
			return suspendHandler.Handle(ctx, lifecycle.Suspend())
		},
		Info: func(ctx context.Context) {
			m.printGuestInfo(ctx, guestAgentSocketPath, watchers)
		},
	}
	// Keep the interface nil for delay-only waits; a typed nil Process reports done.
	if process != nil {
		wait.Process = process
	}
	return launch.WaitForLifecycleProcess(ctx, wait)
}

func (m *manager) waitForLifecycleEvent(ctx context.Context, stage string, delay time.Duration, lifecycle *launch.Lifecycle, suspendHandler suspendHandler, guestAgentSocketPath string, watchers executor.Group) error {
	return m.waitForProcess(ctx, stage, nil, delay, lifecycle, suspendHandler, guestAgentSocketPath, watchers)
}

func (m *manager) saveSuspendStateConnected(ctx context.Context, qmpSocketPath string, client qmpclient.Client, cid int, notifier launch.NotificationSink) error {
	manifest := m.launchManifest
	if manifest == nil {
		return launch.WrapFixedStage("qmp suspend")(fmt.Errorf("suspend manifest is not configured"))
	}

	statePath := launch.VMStatePath(manifest)
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		return launch.WrapFixedStage("qmp suspend")(fmt.Errorf("create directory %q: %w", filepath.Dir(statePath), err))
	}
	if err := os.Remove(statePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return launch.WrapFixedStage("qmp suspend")(fmt.Errorf("remove stale vm state %q: %w", statePath, err))
	}
	migrateCtx, cancel := m.migrationContext(ctx)
	defer cancel()
	if err := qmpclient.SaveToFile(migrateCtx, client, statePath); err != nil {
		return launch.WrapFixedStage("qmp suspend")(err)
	}

	state := launch.SuspendState{
		HostName:      manifest.Identity.HostName,
		QMPSocketPath: qmpSocketPath,
		VMStatePath:   statePath,
		CID:           cid,
		Status:        "saved",
	}
	if err := launch.WriteSuspendStateData(manifest, state); err != nil {
		return launch.WrapFixedStage("qmp suspend")(err)
	}
	notifyRuntimeSuspend(ctx, notifier, state)
	return nil
}

func joinDeferredError(target *error, fn func() error) {
	next := fn()
	if next == nil {
		return
	}
	if *target == nil {
		*target = next
		return
	}
	*target = errors.Join(*target, next)
}
