package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shazow/virtle/internal/manager/control"
	"github.com/shazow/virtle/internal/manager/launch"
)

type fakeSuspendRequester struct {
	err error
}

func (r fakeSuspendRequester) RequestAndWait(context.Context) error {
	return r.err
}

func TestMarkReadyAndStatus(t *testing.T) {
	state := newState(control.RuntimeStarting)
	markReady(state)
	stats := launch.NewStats(time.Now())
	stats.Timer(launch.TimerBootStarted, time.Now().Add(time.Second))
	got := status(state, 7, control.StatusPaths{ControlSocket: "/tmp/virtle.sock"}, stats)
	if got.State != control.RuntimeReady || got.CID != 7 || got.Paths.ControlSocket != "/tmp/virtle.sock" {
		t.Fatalf("status: %#v", got)
	}
	if got.Stats.StartedToBoot == "" {
		t.Fatalf("expected stats conversion in status: %#v", got.Stats)
	}
}

func TestQueueSuspendTransitionsState(t *testing.T) {
	state := newState(control.RuntimeReady)
	if err := queueSuspend(context.Background(), state, fakeSuspendRequester{}, func(error) bool { return false }); err != nil {
		t.Fatalf("queue suspend: %v", err)
	}
	if got := state.Current(); got != control.RuntimeSuspended {
		t.Fatalf("state: got %s want %s", got, control.RuntimeSuspended)
	}
}

func TestQueueSuspendAllowsSavedSuspendExit(t *testing.T) {
	savedErr := errors.New("saved suspend")
	state := newState(control.RuntimeReady)
	err := queueSuspend(context.Background(), state, fakeSuspendRequester{err: savedErr}, func(err error) bool {
		return errors.Is(err, savedErr)
	})
	if err != nil {
		t.Fatalf("queue suspend: %v", err)
	}
	if got := state.Current(); got != control.RuntimeSuspended {
		t.Fatalf("state: got %s want %s", got, control.RuntimeSuspended)
	}
}

func TestQueueSuspendPropagatesUnexpectedError(t *testing.T) {
	suspendErr := errors.New("failed")
	state := newState(control.RuntimeReady)
	err := queueSuspend(context.Background(), state, fakeSuspendRequester{err: suspendErr}, func(error) bool {
		return false
	})
	if !errors.Is(err, suspendErr) {
		t.Fatalf("error: got %v want %v", err, suspendErr)
	}
	if got := state.Current(); got != control.RuntimeSuspending {
		t.Fatalf("state: got %s want %s", got, control.RuntimeSuspending)
	}
}
