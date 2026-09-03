// Package vmm is the qemu backend's VM machinery: the host-side
// launcher lifecycle behind backend/qemu and the virtle CLI. It is named
// for its owner — backend-specific details such as the suspend-state
// version live here as plain constants rather than plumbed configuration.
//
// It takes a validated launch manifest, prepares runtime directories and
// volume images, starts the supporting host processes, and waits for QMP
// readiness. Teardown also lives here: balloon controller tasks stop first,
// helper daemons are shut down, and QEMU is asked to exit through QMP before
// any forced process cleanup is used.
package vmm

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/shazow/virtle/backend/qemu/internal/hotplug"
	"github.com/shazow/virtle/backend/qemu/internal/launch"
	"github.com/shazow/virtle/backend/qemu/internal/qga"
	"github.com/shazow/virtle/backend/qemu/internal/qmpclient"
	"github.com/shazow/virtle/internal/executor"
	"github.com/shazow/virtle/internal/manifest"
)

const (
	defaultShutdownDelay = 15 * time.Second
	// defaultWriteBackTimeout bounds the whole teardown write-back phase:
	// reconnecting to the guest agent and copying changed files back to the
	// host. Individual guest commands are bounded separately.
	defaultWriteBackTimeout = time.Minute
	// defaultGuestInfoTimeout bounds collecting guest diagnostics: waiting
	// for the guest agent and running the process listing.
	defaultGuestInfoTimeout = 10 * time.Second
)

type manager struct {
	// launchManifest is bound once in startWithPlan; one manager serves
	// exactly one manifest.
	launchManifest *manifest.Manifest
	hotplugRuntime *hotplug.Runtime

	locker              launch.Locker
	vsockCIDChecker     launch.VSockCIDChecker
	runner              launch.Runner
	socketWaiter        launch.SocketWaiter
	qmpDialer           qmpclient.Dialer
	guestAgentDialer    qga.Dialer
	logger              *slog.Logger
	balloonLogger       *slog.Logger
	consoleOutput       io.Writer
	shutdownDelay       time.Duration
	qmpRetryDelay       time.Duration
	qmpConnectTimeout   time.Duration
	qmpQuitTimeout      time.Duration
	qmpMigrationTimeout time.Duration
	notifier            launch.NotificationSink
}

func newManagerFromConfig(config Config) *manager {
	config = mergeConfig(DefaultConfig(), config)
	logger := config.Logger.With("package", "vmm")
	runner := config.Runner
	if runner == nil {
		runner = &executor.Runner{Logger: logger}
	}
	return &manager{
		locker:              config.Locker,
		vsockCIDChecker:     config.VSockCIDChecker,
		runner:              runner,
		socketWaiter:        config.SocketWaiter,
		qmpDialer:           config.QMPDialer,
		guestAgentDialer:    config.GuestAgentDialer,
		logger:              logger,
		balloonLogger:       config.Logger.With("package", "balloon"),
		consoleOutput:       config.ConsoleOutput,
		shutdownDelay:       config.ShutdownDelay,
		qmpRetryDelay:       config.QMPRetryDelay,
		qmpConnectTimeout:   config.QMPConnectTimeout,
		qmpQuitTimeout:      config.QMPQuitTimeout,
		qmpMigrationTimeout: config.QMPMigrationTimeout,
		notifier:            config.Notifier,
	}
}

func (m *manager) planLaunch(spec launch.Spec) (*launch.Plan, error) {
	cfg := spec.Manifest
	options := spec.Options
	resumeMode, err := launch.NormalizeResumeMode(options.Resume)
	if err != nil {
		return nil, &launch.StageError{Stage: "preflight", Err: err}
	}
	resumeState, err := launch.ResolveResumeState(cfg, resumeMode, StateVersion)
	if err != nil {
		return nil, &launch.StageError{Stage: "restore", Err: err}
	}
	notifier := m.notifier
	if notifier == nil {
		notifier = newCommandNotifier(cfg, m.logger, m.runner)
	}
	plan, err := launch.BuildPlan(spec, resumeState, notifier)
	if err != nil {
		return nil, &launch.StageError{Stage: "preflight", Err: err}
	}
	return plan, nil
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
		return launch.WrapStage("restore", err)
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

func (m *manager) startManagedProcess(cmd *exec.Cmd) (*executor.Process, error) {
	process, err := m.runner.Start(cmd)
	if err != nil {
		return nil, err
	}
	process.SetGracePeriod(m.shutdownDelay)
	return process, nil
}

func (m *manager) startRuns(cid int) (executor.Group, error) {
	mf := m.launchManifest
	runs, err := mf.ResolvedRuns(cid)
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
			m.logger.Debug("starting run", "index", i)
		}
		cmd := executor.Command(run.Exec[0], run.Exec[1:], run.Env)
		cmd.Dir = run.Dir
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		process, err := m.startManagedProcess(cmd)
		if err != nil {
			_ = started.StopAll(context.Background())
			return executor.Group{}, &launch.StageError{Stage: "run startup", Err: err}
		}
		started.Add(process)
	}

	return started, nil
}

// quitFreshQMP dials a new QMP connection and quits QEMU through it, for
// teardown paths whose long-lived monitor has been poisoned.
func (m *manager) quitFreshQMP(ctx context.Context, socketPath string) error {
	client, err := m.effectiveQMPDialer().Dial(ctx, socketPath, m.effectiveQMPConnectTimeout())
	if err != nil {
		return fmt.Errorf("redial qmp for quit: %w", err)
	}
	defer client.Disconnect()
	return client.Quit(ctx)
}

func (m *manager) waitForQMP(ctx context.Context, socketPath string, watchers executor.Group) (qmpclient.Client, error) {
	return launch.WaitForQMP(ctx, launch.QMPWait{
		Stage:          "vm startup",
		SocketPath:     socketPath,
		SocketWaiter:   m.socketWaiter,
		Dialer:         m.effectiveQMPDialer(),
		ConnectTimeout: m.effectiveQMPConnectTimeout(),
		RetryDelay:     m.effectiveQMPRetryDelay(),
		PollDelay:      defaultSocketPollInterval,
		Watchers:       watchers,
	})
}

func (m *manager) waitForSockets(ctx context.Context, stage string, socketPaths []string, watchers executor.Group) error {
	return launch.WaitForSockets(ctx, launch.SocketWait{
		Stage:        stage,
		SocketPaths:  socketPaths,
		SocketWaiter: m.socketWaiter,
		PollDelay:    defaultSocketPollInterval,
		Watchers:     watchers,
	})
}

// The effective* accessors apply the package defaults for fields that tests
// leave unset when they build a manager literal directly.

func (m *manager) effectiveQMPDialer() qmpclient.Dialer {
	if m.qmpDialer != nil {
		return m.qmpDialer
	}
	return &qmpclient.SocketMonitorDialer{}
}

func (m *manager) effectiveGuestAgentDialer() qga.Dialer {
	if m.guestAgentDialer != nil {
		return m.guestAgentDialer
	}
	return &qga.SocketDialer{}
}

func (m *manager) effectiveQMPRetryDelay() time.Duration {
	if m.qmpRetryDelay > 0 {
		return m.qmpRetryDelay
	}
	return defaultQMPRetryDelay
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

// Handle services one queued suspend request, reporting the outcome to the
// coordinator's waiters.
func (h *launchSuspendHandler) Handle(ctx context.Context, coordinator *launch.SuspendCoordinator) error {
	coordinator.Begin()
	err := h.saveAndExit(ctx)
	coordinator.Complete(err)
	return err
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

func (m *manager) saveSuspendStateConnected(ctx context.Context, qmpSocketPath string, client qmpclient.Client, cid int, notifier launch.NotificationSink) error {
	mf := m.launchManifest
	if mf == nil {
		return launch.WrapStage("qmp suspend", fmt.Errorf("suspend manifest is not configured"))
	}

	statePath, err := launch.PrepareVMStateFile(mf)
	if err != nil {
		return launch.WrapStage("qmp suspend", err)
	}
	migrateCtx, cancel := m.migrationContext(ctx)
	defer cancel()
	if err := qmpclient.SaveToFile(migrateCtx, client, statePath); err != nil {
		return launch.WrapStage("qmp suspend", err)
	}

	state := launch.SuspendState{
		Version:       StateVersion,
		HostName:      mf.Identity.HostName,
		QMPSocketPath: qmpSocketPath,
		VMStatePath:   statePath,
		CID:           cid,
		Status:        launch.SuspendStatusSaved,
	}
	if err := launch.WriteSuspendStateData(mf, state); err != nil {
		return launch.WrapStage("qmp suspend", err)
	}
	notifyRuntimeSuspend(ctx, notifier, state)
	return nil
}
