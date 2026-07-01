package executor_test

import (
	"errors"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/shazow/virtle/internal/executor"
	"github.com/shazow/virtle/internal/executor/executortest"
)

func TestProcessCachesWaitResult(t *testing.T) {
	handle := &executortest.Process{OverrideName: "worker"}
	process := executor.Wrap(handle)
	handle.Complete(errors.New("done"))

	for i := 0; i < 2; i++ {
		err := process.Wait()
		if err == nil || err.Error() != "done" {
			t.Fatalf("wait %d: got %v", i, err)
		}
	}
}

func TestProcessPollExit(t *testing.T) {
	handle := &executortest.Process{OverrideName: "worker"}
	process := executor.Wrap(handle)
	if exited, err := process.PollExit(); exited || err != nil {
		t.Fatalf("poll before exit: exited=%v err=%v", exited, err)
	}

	handle.Complete(errors.New("failed"))
	<-process.Done()

	exited, err := process.PollExit()
	if !exited || err == nil || err.Error() != "failed" {
		t.Fatalf("poll after exit: exited=%v err=%v", exited, err)
	}
}

func TestProcessStopUsesShutdownCallback(t *testing.T) {
	handle := &executortest.Process{OverrideName: "worker"}
	process := executor.Wrap(handle)
	called := false
	process.SetShutdown(func() error {
		called = true
		handle.Complete(nil)
		return nil
	})

	if err := process.Stop(time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if !called {
		t.Fatal("expected shutdown callback")
	}
	if signals := handle.Signals(); len(signals) != 0 {
		t.Fatalf("unexpected signals: %v", signals)
	}
}

func TestProcessStopSignalsThenKills(t *testing.T) {
	handle := &executortest.Process{OverrideName: "worker", IgnoreSignals: true}
	process := executor.Wrap(handle)

	if err := process.Stop(time.Millisecond); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if got, want := handle.EventKinds(), []executortest.EventKind{executortest.EventSignal, executortest.EventKill}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events: got %v want %v", got, want)
	}
}

func TestProcessStopReportsShutdownAndSignalErrors(t *testing.T) {
	handle := &executortest.Process{OverrideName: "worker", SignalErr: errors.New("signal failed")}
	process := executor.Wrap(handle)
	process.SetShutdown(func() error {
		return errors.New("shutdown failed")
	})

	err := process.Stop(time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "shutdown worker") || !strings.Contains(err.Error(), "stop worker") {
		t.Fatalf("expected joined shutdown and stop error, got %v", err)
	}
}

func TestProcessKillAndWait(t *testing.T) {
	handle := &executortest.Process{OverrideName: "worker"}
	process := executor.Wrap(handle)

	if err := process.KillAndWait(); err != nil {
		t.Fatalf("kill and wait: %v", err)
	}
	if got, want := handle.EventKinds(), []executortest.EventKind{executortest.EventKill}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events: got %v want %v", got, want)
	}
}

func TestSignalProcessGroupIgnoresNonPositivePID(t *testing.T) {
	for _, pid := range []int{0, -1} {
		if err := executor.SignalProcessGroup(pid, syscall.SIGTERM); err != nil {
			t.Fatalf("signal pid %d: %v", pid, err)
		}
	}
}
