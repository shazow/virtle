// Package backendtest provides a conformance test for backend.Backend
// implementations — the nettest.TestConn / fstest.TestFS of this API. A
// backend's own tests call TestBackend with a constructor that yields a
// fresh backend and a launchable Spec:
//
//	func TestConformance(t *testing.T) {
//		backendtest.TestBackend(t, func(t *testing.T) (backend.Backend, *vm.Spec) {
//			return &qemu.Backend{RemoteControl: qemu.QGA{}}, testSpec(t)
//		})
//	}
package backendtest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/vm"
)

// exitTimeout bounds how long a conformance step waits for an instance
// to report exit after it was asked to stop.
const exitTimeout = 2 * time.Minute

// TestBackend exercises the backend.Backend and backend.Instance contract:
// Start, Done/Err/Wait, Shutdown, Kill, RemoteControl, and the capability
// pairs the implementation asserts to (Suspender with Resumer,
// MemoryResizer). Each subtest starts its own instance from newBackend,
// which must return a backend and a Spec the backend can start; the
// instance is stopped before the subtest ends. DeviceAttacher and
// ConsoleProvider need backend-specific inputs and are not exercised.
func TestBackend(t *testing.T, newBackend func(t *testing.T) (backend.Backend, *vm.Spec)) {
	t.Helper()
	t.Run("Start", func(t *testing.T) {
		inst := start(t, newBackend)
		if inst.Done() == nil {
			t.Fatal("Done returned a nil channel")
		}
		select {
		case <-inst.Done():
			t.Fatal("Done is closed immediately after Start")
		default:
		}
		if err := inst.Err(); err != nil {
			t.Fatalf("Err before exit = %v, want nil", err)
		}
		stop(t, inst)
	})

	t.Run("Shutdown", func(t *testing.T) {
		inst := start(t, newBackend)
		ctx, cancel := context.WithTimeout(context.Background(), exitTimeout)
		defer cancel()
		if err := inst.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
		requireDone(t, inst)
		if err := inst.Wait(context.Background()); err != inst.Err() {
			t.Errorf("Wait after exit = %v, Err = %v; want them to agree", err, inst.Err())
		}
		if err := inst.Shutdown(ctx); err != nil {
			t.Errorf("second Shutdown = %v, want nil", err)
		}
	})

	t.Run("Kill", func(t *testing.T) {
		inst := start(t, newBackend)
		if err := inst.Kill(); err != nil {
			t.Fatalf("Kill: %v", err)
		}
		requireDone(t, inst)
	})

	t.Run("WaitContext", func(t *testing.T) {
		inst := start(t, newBackend)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := inst.Wait(ctx); !errors.Is(err, context.Canceled) {
			t.Errorf("Wait with canceled ctx = %v, want context.Canceled", err)
		}
		select {
		case <-inst.Done():
			t.Error("Done closed by a canceled Wait")
		default:
		}
		stop(t, inst)
	})

	t.Run("RemoteControl", func(t *testing.T) {
		inst := start(t, newBackend)
		defer stop(t, inst)
		guest, err := inst.RemoteControl()
		switch {
		case err != nil && !errors.Is(err, errors.ErrUnsupported):
			t.Fatalf("RemoteControl error %v does not wrap errors.ErrUnsupported", err)
		case err != nil:
			return
		case guest == nil:
			t.Fatal("RemoteControl returned a nil guest without error")
		}
		if err := guest.Close(); err != nil {
			t.Errorf("guest Close: %v", err)
		}
	})

	t.Run("SuspendResume", func(t *testing.T) {
		b, spec := newBackend(t)
		resumer, ok := b.(backend.Resumer)
		inst := startOn(t, b, spec)
		suspender, hasSuspend := inst.(backend.Suspender)
		if !hasSuspend {
			stop(t, inst)
			t.Skip("instance does not implement backend.Suspender")
		}
		if !ok {
			stop(t, inst)
			t.Fatal("instance implements backend.Suspender but the backend does not implement backend.Resumer")
		}
		if resumer.StateVersion() == "" {
			t.Error("StateVersion is empty")
		}
		ctx, cancel := context.WithTimeout(context.Background(), exitTimeout)
		defer cancel()
		if err := suspender.Suspend(ctx); err != nil {
			t.Fatalf("Suspend: %v", err)
		}
		requireDone(t, inst)

		resumed, err := resumer.Resume(ctx, spec)
		if err != nil {
			t.Fatalf("Resume: %v", err)
		}
		select {
		case <-resumed.Done():
			t.Fatal("resumed instance is already done")
		default:
		}
		stop(t, resumed)
	})

	t.Run("ResizeMemory", func(t *testing.T) {
		b, spec := newBackend(t)
		inst := startOn(t, b, spec)
		defer stop(t, inst)
		resizer, ok := inst.(backend.MemoryResizer)
		if !ok {
			t.Skip("instance does not implement backend.MemoryResizer")
		}
		if spec.Memory == 0 {
			t.Skip("spec has no explicit memory size to resize to")
		}
		// Resizing to the current size is the one request every resizer
		// can honor; a backend whose instance lacks the device reports
		// errors.ErrUnsupported rather than failing arbitrarily.
		err := resizer.ResizeMemory(context.Background(), spec.Memory)
		if err != nil && !errors.Is(err, errors.ErrUnsupported) {
			t.Errorf("ResizeMemory to the current size: %v", err)
		}
	})
}

func start(t *testing.T, newBackend func(t *testing.T) (backend.Backend, *vm.Spec)) backend.Instance {
	t.Helper()
	b, spec := newBackend(t)
	return startOn(t, b, spec)
}

func startOn(t *testing.T, b backend.Backend, spec *vm.Spec) backend.Instance {
	t.Helper()
	inst, err := b.Start(context.Background(), spec)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if inst == nil {
		t.Fatal("Start returned a nil instance without error")
	}
	return inst
}

// stop shuts an instance down at the end of a subtest.
func stop(t *testing.T, inst backend.Instance) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), exitTimeout)
	defer cancel()
	if err := inst.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
	requireDone(t, inst)
}

// requireDone asserts that Done is closed (or closes promptly) and that
// Wait returns without blocking afterwards.
func requireDone(t *testing.T, inst backend.Instance) {
	t.Helper()
	select {
	case <-inst.Done():
	case <-time.After(exitTimeout):
		t.Fatal("Done not closed after the instance was stopped")
	}
	ctx, cancel := context.WithTimeout(context.Background(), exitTimeout)
	defer cancel()
	if err := inst.Wait(ctx); errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("Wait blocked after Done was closed")
	}
}
