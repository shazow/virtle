package executor_test

import (
	"context"
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

func TestProcessWaitContext(t *testing.T) {
	handle := &executortest.Process{OverrideName: "worker"}
	process := executor.Wrap(handle)

	ctx, cancel := context.WithTimeoutCause(context.Background(), time.Millisecond, errors.New("gave up"))
	defer cancel()
	if err := process.WaitContext(ctx); err == nil || err.Error() != "gave up" {
		t.Fatalf("expected ctx cause, got %v", err)
	}

	handle.Complete(errors.New("exit result"))
	if err := process.WaitContext(context.Background()); err == nil || err.Error() != "exit result" {
		t.Fatalf("expected wait result, got %v", err)
	}
}

func TestProcessStopEndedContextSkipsGraceAndKills(t *testing.T) {
	handle := &executortest.Process{OverrideName: "worker", IgnoreSignals: true}
	process := executor.Wrap(handle)
	// A generous grace period that would stall the test if Stop waited it out.
	process.SetGracePeriod(time.Hour)
	shutdownCalled := false
	process.SetShutdown(func() error {
		shutdownCalled = true
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := process.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("stop took %s; ended ctx should skip the grace ladder", elapsed)
	}
	if shutdownCalled {
		t.Fatal("ended ctx should skip the shutdown callback")
	}
	if got, want := handle.EventKinds(), []executortest.EventKind{executortest.EventKill}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events: got %v want %v", got, want)
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

	if err := process.Stop(context.Background()); err != nil {
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
	process.SetGracePeriod(time.Millisecond)

	if err := process.Stop(context.Background()); err != nil {
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

	err := process.Stop(context.Background())
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

func TestStopBoundsPostKillWait(t *testing.T) {
	// A process that ignores SIGTERM and SIGKILL (e.g. wedged in
	// uninterruptible sleep) must not hang Stop forever.
	fake := &executortest.Process{OverrideName: "wedged", IgnoreSignals: true, IgnoreKill: true}
	process := executor.Wrap(fake)
	process.SetGracePeriod(10 * time.Millisecond)

	done := make(chan error, 1)
	go func() {
		done <- process.Stop(context.Background())
	}()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "process did not exit") {
			t.Fatalf("expected bounded-wait error, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return for an unkillable process")
	}

	fake.Complete(nil)
}
