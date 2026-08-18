package launch

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/shazow/virtle/internal/executor"
	"github.com/shazow/virtle/internal/executor/executortest"
)

func TestWaitForSocketsUsesSocketWaiter(t *testing.T) {
	waiter := &fakeAsyncSocketWaiter{}
	if err := WaitForSockets(context.Background(), SocketWait{
		Stage:        "virtiofs startup",
		SocketPaths:  []string{"a.sock", "b.sock"},
		SocketWaiter: waiter,
		PollDelay:    time.Millisecond,
	}); err != nil {
		t.Fatalf("wait for sockets: %v", err)
	}
	if got, want := waiter.paths, []string{"a.sock", "b.sock"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("socket paths: got %v want %v", got, want)
	}
}

func TestWaitForSocketsWrapsSocketError(t *testing.T) {
	waitErr := errors.New("socket failed")
	err := WaitForSockets(context.Background(), SocketWait{
		Stage:        "guest agent",
		SocketWaiter: &fakeAsyncSocketWaiter{err: waitErr},
		PollDelay:    time.Millisecond,
	})
	var stageErr *StageError
	if !errors.As(err, &stageErr) || stageErr.Stage != "guest agent" || !errors.Is(err, waitErr) {
		t.Fatalf("wrapped err: got %v", err)
	}
}

func TestWaitForSocketsChecksWatcherExitWhileWaiting(t *testing.T) {
	process := &executortest.Process{OverrideName: "qemu", Exited: true}
	err := WaitForSockets(context.Background(), SocketWait{
		Stage:     "virtiofs startup",
		Watchers:  executor.NewGroup(process.Process()),
		PollDelay: time.Millisecond,
		SocketWaiter: &fakeAsyncSocketWaiter{
			block: true,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "virtiofs startup: qemu exited unexpectedly") {
		t.Fatalf("watcher exit error: got %v", err)
	}
}

func TestWaitForSocketsWrapsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := WaitForSockets(ctx, SocketWait{
		Stage:        "guest agent",
		PollDelay:    time.Millisecond,
		SocketWaiter: &fakeAsyncSocketWaiter{block: true},
	})
	var stageErr *StageError
	if !errors.As(err, &stageErr) || stageErr.Stage != "guest agent" || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel err: got %v", err)
	}
}

type fakeAsyncSocketWaiter struct {
	paths []string
	err   error
	block bool
}

func (w *fakeAsyncSocketWaiter) Wait(ctx context.Context, socketPaths []string) error {
	w.paths = append([]string(nil), socketPaths...)
	if w.block {
		<-ctx.Done()
		return ctx.Err()
	}
	return w.err
}
