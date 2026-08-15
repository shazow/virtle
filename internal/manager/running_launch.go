package manager

import (
	"context"
	"errors"

	"github.com/shazow/virtle/internal/manager/launch"
	runtimepkg "github.com/shazow/virtle/internal/manager/runtime"
	"github.com/shazow/virtle/internal/qmpclient"
)

var errRuntimeNotStarted = errors.New("runtime is not started")

type runningLaunch struct {
	ctx            context.Context
	runtime        *runtimepkg.Core
	plan           *launch.Plan
	stats          *launch.Stats
	qmp            qmpclient.Client
	lifecycle      *launch.Lifecycle
	suspendHandler suspendHandler
	processes      *launch.ProcessSet
}

func (r *runningLaunch) Close() error {
	if r == nil || r.runtime == nil {
		return nil
	}
	return r.runtime.Close()
}

func (m *manager) waitForRunningLaunch(ctx context.Context, running *runningLaunch, mode launch.WaitMode) error {
	if running == nil {
		return &launch.StageError{Stage: "runtime wait", Err: errRuntimeNotStarted}
	}
	waitCtx := ctx
	if running.ctx != nil {
		waitCtx = running.ctx
	}
	waitPlan := launch.PlanForWaitMode(running.plan, mode)
	err := m.waitForLaunchForeground(waitCtx, waitPlan, running.stats, running.lifecycle, running.suspendHandler, running.processes)
	if err != nil && launch.IsSavedSuspendExit(err) && running.runtime != nil {
		running.runtime.MarkSavedSuspend()
	}
	return err
}
