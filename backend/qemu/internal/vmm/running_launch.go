package vmm

import (
	"context"
	"errors"

	"github.com/shazow/virtle/backend/qemu/internal/launch"
	"github.com/shazow/virtle/backend/qemu/internal/qmpclient"
	runtimepkg "github.com/shazow/virtle/backend/qemu/internal/runtime"
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

func (m *manager) waitForRunningLaunch(ctx context.Context, running *runningLaunch, session SessionOptions) error {
	if running == nil {
		return &launch.StageError{Stage: "runtime wait", Err: errRuntimeNotStarted}
	}
	waitCtx := ctx
	if running.ctx != nil {
		waitCtx = running.ctx
	}
	mode := launch.WaitVM
	if session.SSH {
		mode = launch.WaitSSH
	}
	waitPlan := launch.PlanForWaitMode(running.plan, mode)
	err := m.waitForLaunchForeground(waitCtx, waitPlan, running.stats, running.lifecycle, running.suspendHandler, running.processes, session)
	if err != nil && launch.IsSavedSuspendExit(err) && running.runtime != nil {
		running.runtime.MarkSavedSuspend()
	}
	return err
}
