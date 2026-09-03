package runtime

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/shazow/virtle/backend/qemu/internal/launch"
	"github.com/shazow/virtle/backend/qemu/internal/qmpclient"
	"github.com/shazow/virtle/internal/control"
	"github.com/shazow/virtle/internal/manifest"
)

type Core struct {
	manifest         *manifest.Manifest
	paths            launch.RuntimePaths
	cid              int
	stats            *launch.Stats
	qmp              qmpclient.Client
	suspendRequests  *launch.SuspendCoordinator
	processes        *launch.ProcessSet
	writeBackTimeout time.Duration
	logger           *slog.Logger
	savedSuspendExit func(error) bool
	writeBack        func(context.Context) error
	cleanup          func() error
	savedSuspend     atomic.Bool

	state   *state
	closer  *closer
	control *control.Server
}

func New(config Config) *Core {
	state := newState(control.RuntimeStarting)
	return &Core{
		manifest:         config.Manifest,
		paths:            config.Paths,
		cid:              config.CID,
		stats:            config.Stats,
		qmp:              config.QMP,
		suspendRequests:  config.SuspendRequests,
		writeBackTimeout: config.WriteBackTimeout,
		logger:           config.Logger,
		savedSuspendExit: config.SavedSuspendExit,
		processes:        config.Processes,
		writeBack:        config.WriteBack,
		cleanup:          config.Cleanup,
		state:            state,
		closer:           newCloser(state),
	}
}

func (r *Core) SetReady() {
	markReady(r.state)
}

func (r *Core) MarkSavedSuspend() {
	r.savedSuspend.Store(true)
}

func (r *Core) QMP() qmpclient.Client {
	return r.qmp
}

func (r *Core) StartControl(ctx context.Context, handlers control.Handlers) (*control.Server, error) {
	handlers.Core = r
	handlers.Kill = r
	handlers.Shutdown = r
	handlers.Suspend = r
	handlers.Balloon = r
	router, err := control.NewRouter(handlers)
	if err != nil {
		return nil, err
	}
	controlServer, err := startControl(ctx, r.paths.ControlSocket, router, r.logger)
	if err == nil {
		r.control = controlServer
	}
	return controlServer, err
}

// Kill hard-stops QEMU and tears the runtime down. It ignores ctx: a hard
// stop must never be cut short by the requesting peer disconnecting.
func (r *Core) Kill(_ context.Context, _ control.KillRequest) (control.KillResponse, error) {
	var err error
	if r.processes != nil && r.processes.QEMU() != nil {
		err = r.processes.QEMU().KillAndWait()
	}
	return control.KillResponse{}, errors.Join(err, r.Shutdown(context.Background()))
}

func (r *Core) ShutdownRPC(ctx context.Context, _ control.ShutdownRequest) (control.ShutdownResponse, error) {
	return control.ShutdownResponse{}, r.Shutdown(ctx)
}

func (r *Core) Shutdown(ctx context.Context) error {
	return r.closer.Close(ctx, closeActions{
		Processes:        r.processes,
		QMP:              r.qmp,
		WriteBack:        r.writeBack,
		WriteBackTimeout: r.writeBackTimeout,
		SkipWriteBack:    r.savedSuspend.Load(),
		Control:          r.control,
		Cleanup:          r.cleanup,
	})
}

func (r *Core) Wait(ctx context.Context, _ control.WaitRequest) (control.WaitResponse, error) {
	return control.WaitResponse{}, r.state.Wait(ctx)
}

func (r *Core) Status(_ context.Context, _ control.StatusRequest) (control.StatusResponse, error) {
	pid := 0
	if r.processes != nil && r.processes.QEMU() != nil {
		pid = r.processes.QEMU().PID()
	}
	return status(r.state, r.cid, pid, control.StatusPaths{
		ControlSocket:      r.paths.ControlSocket,
		MonitorSocket:      r.paths.QMPSocket,
		GuestControlSocket: r.paths.GuestAgentSocket,
		ReadySocket:        r.paths.SSHReadySocket,
	}, r.stats), nil
}

func (r *Core) Suspend(ctx context.Context, req control.SuspendRequest) (control.SuspendResponse, error) {
	if r.suspendRequests == nil {
		return control.SuspendResponse{}, control.FailedPrecondition(errSuspendNotReady)
	}
	err := queueSuspend(ctx, r.state, r.suspendRequests, r.isSavedSuspendExit)
	if errors.Is(err, errSuspendNotReady) {
		return control.SuspendResponse{}, control.FailedPrecondition(err)
	}
	if err != nil {
		return control.SuspendResponse{}, err
	}
	return control.SuspendResponse{Saved: true, VMStatePath: launch.VMStatePath(r.manifest)}, nil
}

func (r *Core) Balloon(ctx context.Context, req control.BalloonRequest) (control.BalloonResponse, error) {
	resp, err := balloon(ctx, r.manifest.QEMU.Devices.Balloon, r.qmp, req)
	if errors.Is(err, errBalloonNotConfigured) {
		return control.BalloonResponse{}, control.FailedPrecondition(err)
	}
	return resp, err
}

func (r *Core) isSavedSuspendExit(err error) bool {
	return r.savedSuspendExit != nil && r.savedSuspendExit(err)
}
