package launch

import (
	"context"
	"errors"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestLifecycleMapsSignalsToEvents(t *testing.T) {
	signals := make(chan os.Signal, 3)
	ctx, cancel := context.WithCancel(context.Background())
	lifecycle := NewSignalLifecycle(signals, cancel)
	defer lifecycle.Stop()

	signals <- syscall.SIGUSR1
	select {
	case <-lifecycle.Info():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for info signal")
	}

	signals <- syscall.SIGTSTP
	select {
	case <-lifecycle.Suspend().Notify():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for suspend signal")
	}

	signals <- syscall.SIGTERM
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for cancellation signal")
	}

	lifecycle.Stop()
}

func TestLifecycleStopCallsCallback(t *testing.T) {
	stopped := false
	lifecycle := NewLifecycle(nil, func() { stopped = true }, nil)
	lifecycle.Stop()
	if !stopped {
		t.Fatal("stop signal callback was not called")
	}
}

func TestSuspendCoordinatorRequestAndWait(t *testing.T) {
	wantErr := errors.New("saved")
	coordinator := NewSuspendCoordinator()
	done := make(chan error, 1)
	go func() {
		done <- coordinator.RequestAndWait(context.Background())
	}()

	select {
	case <-coordinator.Notify():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for suspend notification")
	}
	coordinator.Begin()
	coordinator.Complete(wantErr)

	select {
	case err := <-done:
		if !errors.Is(err, wantErr) {
			t.Fatalf("unexpected wait error: got %v want %v", err, wantErr)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for suspend completion")
	}
}

func TestHandleQueuedSuspend(t *testing.T) {
	lifecycle := NewLifecycle(nil, nil, nil)
	defer lifecycle.Stop()
	wantErr := errors.New("suspend handled")
	lifecycle.Suspend().Request()

	err := HandleQueuedSuspend(context.Background(), lifecycle, func(ctx context.Context, coordinator *SuspendCoordinator) error {
		if ctx == nil {
			t.Fatal("expected context")
		}
		if coordinator != lifecycle.Suspend() {
			t.Fatal("unexpected coordinator")
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("queued suspend error: got %v want %v", err, wantErr)
	}
}

func TestHandleQueuedSuspendReturnsNilWhenNoRequest(t *testing.T) {
	lifecycle := NewLifecycle(nil, nil, nil)
	defer lifecycle.Stop()
	err := HandleQueuedSuspend(context.Background(), lifecycle, func(context.Context, *SuspendCoordinator) error {
		t.Fatal("handler should not run without queued request")
		return nil
	})
	if err != nil {
		t.Fatalf("queued suspend without request: %v", err)
	}
}
