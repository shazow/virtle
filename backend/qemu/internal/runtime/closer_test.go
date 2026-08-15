package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shazow/virtle/backend/qemu/internal/launch"
	"github.com/shazow/virtle/internal/control"
)

func TestCloserRunsOnceAndTracksStoppedState(t *testing.T) {
	state := newState(control.RuntimeReady)
	closer := newCloser(state)
	calls := 0

	if err := closer.Close(closeActions{
		Cleanup: func() error {
			calls++
			if got := state.Current(); got != control.RuntimeStopping {
				t.Fatalf("state during close: got %q want %q", got, control.RuntimeStopping)
			}
			return nil
		},
	}); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := closer.Close(closeActions{
		Cleanup: func() error {
			calls++
			return errors.New("second close should not run")
		},
	}); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if calls != 1 {
		t.Fatalf("close calls: got %d want 1", calls)
	}
	if got := state.Current(); got != control.RuntimeStopped {
		t.Fatalf("state after close: got %q want %q", got, control.RuntimeStopped)
	}
}

func TestCloserReturnsFirstCloseError(t *testing.T) {
	wantErr := errors.New("close failed")
	closer := newCloser(newState(control.RuntimeReady))
	if err := closer.Close(closeActions{Cleanup: func() error { return wantErr }}); !errors.Is(err, wantErr) {
		t.Fatalf("close error: got %v want %v", err, wantErr)
	}
	if err := closer.Close(closeActions{Cleanup: func() error { return nil }}); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestCloseActionsRunInShutdownOrder(t *testing.T) {
	var calls []string
	actions := closeActions{
		WriteBackTimeout: time.Second,
		WriteBack: func(context.Context) error {
			calls = append(calls, "writeback")
			return nil
		},
		shutdownResources: shutdownResources{
			Processes: launch.NewProcessSet(),
			QMP: closeQMPFunc(func() error {
				calls = append(calls, "qmp")
				return nil
			}),
		},
		Cleanup: func() error {
			calls = append(calls, "cleanup")
			return nil
		},
	}

	if err := actions.Run(); err != nil {
		t.Fatalf("run close actions: %v", err)
	}
	want := []string{"writeback", "qmp", "cleanup"}
	if len(calls) != len(want) {
		t.Fatalf("calls: got %#v want %#v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("calls: got %#v want %#v", calls, want)
		}
	}
}

func TestCloseActionsSkipWriteBack(t *testing.T) {
	called := false
	actions := closeActions{
		WriteBack: func(context.Context) error {
			called = true
			return nil
		},
		SkipWriteBack: true,
	}
	if err := actions.Run(); err != nil {
		t.Fatalf("run close actions: %v", err)
	}
	if called {
		t.Fatal("write-back ran despite skip flag")
	}
}

type closeQMPFunc func() error

func (fn closeQMPFunc) Disconnect() error {
	return fn()
}
