package runtime

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/shazow/virtle/backend/qemu/internal/launch"
	"github.com/shazow/virtle/internal/control"
	"github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/internal/qmpclient"
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

func New(config RuntimeConfig) *Core {
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
	handlers.Suspend = r
	handlers.Balloon = r
	router, err := control.NewRouter(handlers)
	if err != nil {
		return nil, err
	}
	controlServer, err := StartControl(ctx, r.paths.ControlSocket, router, r.logger)
	if err == nil {
		r.control = controlServer
	}
	return controlServer, err
}

func (r *Core) Close() error {
	return r.closer.Close(closeActions{
		shutdownResources: shutdownResources{
			Processes: r.processes,
			QMP:       r.qmp,
		},
		WriteBack:        r.writeBack,
		WriteBackTimeout: r.writeBackTimeout,
		SkipWriteBack:    r.savedSuspend.Load(),
		Control:          r.control,
		Cleanup:          r.cleanup,
	})
}

func (r *Core) Status(ctx context.Context, req control.StatusRequest) (control.StatusResponse, error) {
	_ = ctx
	return status(r.state, r.cid, control.StatusPaths{
		ControlSocket:    r.paths.ControlSocket,
		QMPSocket:        r.paths.QMPSocket,
		GuestAgentSocket: r.paths.GuestAgentSocket,
		SSHReadySocket:   r.paths.SSHReadySocket,
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
