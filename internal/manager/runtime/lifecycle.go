package runtime

import (
	"context"
	"errors"

	"github.com/shazow/virtle/internal/manager/control"
	"github.com/shazow/virtle/internal/manager/launch"
)

var errSuspendNotReady = errors.New("suspend handler is not ready")

type suspendRequester interface {
	RequestAndWait(context.Context) error
}

func markReady(state *state) {
	state.Set(control.RuntimeReady)
}

func status(state *state, cid int, paths control.StatusPaths, stats *launch.Stats) control.StatusResponse {
	return control.StatusResponse{
		State: state.Current(),
		CID:   cid,
		Paths: paths,
		Stats: launch.ControlStats(stats),
	}
}

func queueSuspend(ctx context.Context, state *state, requester suspendRequester, savedSuspendExit func(error) bool) error {
	if requester == nil {
		return errSuspendNotReady
	}
	state.Set(control.RuntimeSuspending)
	err := requester.RequestAndWait(ctx)
	if err != nil && (savedSuspendExit == nil || !savedSuspendExit(err)) {
		return err
	}
	state.Set(control.RuntimeSuspended)
	return nil
}
