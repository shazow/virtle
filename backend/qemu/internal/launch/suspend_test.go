package launch

import (
	"context"
	"errors"
	"testing"
	"time"
)

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
	coordinator := NewSuspendCoordinator()
	wantErr := errors.New("suspend handled")
	coordinator.Request()

	err := HandleQueuedSuspend(context.Background(), coordinator, func(ctx context.Context, got *SuspendCoordinator) error {
		if ctx == nil {
			t.Fatal("expected context")
		}
		if got != coordinator {
			t.Fatal("unexpected coordinator")
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("queued suspend error: got %v want %v", err, wantErr)
	}
}

func TestHandleQueuedSuspendReturnsNilWhenNoRequest(t *testing.T) {
	coordinator := NewSuspendCoordinator()
	err := HandleQueuedSuspend(context.Background(), coordinator, func(context.Context, *SuspendCoordinator) error {
		t.Fatal("handler should not run without queued request")
		return nil
	})
	if err != nil {
		t.Fatalf("queued suspend without request: %v", err)
	}
}
