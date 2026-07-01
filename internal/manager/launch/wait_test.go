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

func TestWaitForLifecycleProcessWaitsForExitStatus(t *testing.T) {
	process := &fakeWaitProcess{done: make(chan struct{})}
	close(process.done)

	if err := WaitForLifecycleProcess(context.Background(), LifecycleProcessWait{
		Stage:     "active session",
		Process:   process,
		PollDelay: time.Millisecond,
	}); err != nil {
		t.Fatalf("wait for process: %v", err)
	}
	if !process.waited {
		t.Fatalf("expected process Wait to be called")
	}
}

func TestWaitForLifecycleProcessWrapsExitError(t *testing.T) {
	waitErr := errors.New("exit status 1")
	process := &fakeWaitProcess{name: "ssh", done: make(chan struct{}), err: waitErr}
	close(process.done)

	err := WaitForLifecycleProcess(context.Background(), LifecycleProcessWait{
		Stage:     "active session",
		Process:   process,
		PollDelay: time.Millisecond,
	})
	var commandErr *CommandError
	if !errors.As(err, &commandErr) || commandErr.Stage != "active session" || commandErr.Command != "ssh" || !errors.Is(err, waitErr) {
		t.Fatalf("wrapped err: got %v", err)
	}
}

func TestWaitForLifecycleProcessHandlesSuspend(t *testing.T) {
	lifecycle := NewLifecycle(nil, nil, nil)
	defer lifecycle.Stop()
	wantErr := errors.New("suspended")
	process := &fakeWaitProcess{done: make(chan struct{})}
	lifecycle.Suspend().Request()

	err := WaitForLifecycleProcess(context.Background(), LifecycleProcessWait{
		Stage:     "active session",
		Process:   process,
		Lifecycle: lifecycle,
		PollDelay: time.Millisecond,
		Suspend: func(context.Context) error {
			return wantErr
		},
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("suspend err: got %v want %v", err, wantErr)
	}
	if process.waited {
		t.Fatalf("did not expect process Wait after lifecycle error")
	}
}

func TestWaitForLifecycleProcessHandlesInfoAndContinues(t *testing.T) {
	lifecycle := NewLifecycle(nil, nil, nil)
	defer lifecycle.Stop()
	process := &fakeWaitProcess{done: make(chan struct{})}
	infoCalled := make(chan struct{}, 1)
	lifecycle.RequestInfo()

	go func() {
		<-infoCalled
		close(process.done)
	}()

	err := WaitForLifecycleProcess(context.Background(), LifecycleProcessWait{
		Stage:     "vm session",
		Process:   process,
		Lifecycle: lifecycle,
		PollDelay: time.Millisecond,
		Info: func(context.Context) {
			infoCalled <- struct{}{}
		},
	})
	if err != nil {
		t.Fatalf("wait after info: %v", err)
	}
	if !process.waited {
		t.Fatalf("expected process Wait after process completion")
	}
}

func TestWaitForLifecycleProcessWrapsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := WaitForLifecycleProcess(ctx, LifecycleProcessWait{
		Stage:     "active session",
		PollDelay: time.Millisecond,
	})
	var stageErr *StageError
	if !errors.As(err, &stageErr) || stageErr.Stage != "active session" || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error: got %v", err)
	}
}

func TestWaitForLifecycleProcessWrapsUnexpectedWatcherExit(t *testing.T) {
	process := &executortest.Process{OverrideName: "qemu", Exited: true}
	err := WaitForLifecycleProcess(context.Background(), LifecycleProcessWait{
		Stage:     "vm session",
		Watchers:  executor.NewGroup(process.Process()),
		PollDelay: time.Millisecond,
	})
	if err == nil || !strings.Contains(err.Error(), "vm session: qemu exited unexpectedly") {
		t.Fatalf("watcher exit error: got %v", err)
	}
}

func TestWaitForLifecycleProcessReturnsWhenDelayCompletes(t *testing.T) {
	if err := WaitForLifecycleProcess(context.Background(), LifecycleProcessWait{Delay: time.Millisecond, PollDelay: time.Millisecond}); err != nil {
		t.Fatalf("wait for delay: %v", err)
	}
}

type fakeWaitProcess struct {
	name   string
	done   chan struct{}
	err    error
	waited bool
}

func (p *fakeWaitProcess) Done() <-chan struct{} { return p.done }

func (p *fakeWaitProcess) Wait() error {
	p.waited = true
	return p.err
}

func (p *fakeWaitProcess) Name() string { return p.name }
