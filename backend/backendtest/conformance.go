package backendtest

import (
	"context"
	"errors"
	"testing"

	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/units"
	"github.com/shazow/virtle/vm"
)

// TestBackend runs the backend contract against fresh machines from start.
func TestBackend(t *testing.T, start func(t *testing.T) (backend.Backend, *vm.Spec)) {
	t.Helper()
	startMachine := func(t *testing.T) backend.Machine {
		t.Helper()
		b, spec := start(t)
		m, err := b.Start(context.Background(), spec)
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		return m
	}

	t.Run("shutdown", func(t *testing.T) {
		m := startMachine(t)
		if err := m.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
		<-m.Done()
		if err := m.Err(); err != nil {
			t.Fatalf("Err: %v", err)
		}
		if err := m.Wait(context.Background()); err != nil {
			t.Fatalf("Wait after exit: %v", err)
		}
		if err := m.Shutdown(context.Background()); err != nil {
			t.Fatalf("repeated Shutdown: %v", err)
		}
	})

	t.Run("kill", func(t *testing.T) {
		m := startMachine(t)
		if err := m.Kill(); err != nil {
			t.Fatalf("Kill: %v", err)
		}
		<-m.Done()
		// Kill may legitimately record a non-nil exit; Wait must agree with Err.
		if got, want := m.Wait(context.Background()), m.Err(); !errors.Is(got, want) {
			t.Fatalf("Wait after Kill = %v, want Err %v", got, want)
		}
	})

	t.Run("shutdown_expired_context", func(t *testing.T) {
		m := startMachine(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_ = m.Shutdown(ctx) // must fall back to a hard stop
		<-m.Done()
	})

	t.Run("guest", func(t *testing.T) {
		m := startMachine(t)
		defer m.Kill()
		g, err := m.RemoteControl()
		if errors.Is(err, errors.ErrUnsupported) {
			t.Skip("remote control unsupported")
		}
		if err != nil {
			t.Fatalf("RemoteControl: %v", err)
		}
		if err := g.Run(context.Background(), &vm.GuestCmd{Path: "true"}); errors.Is(err, errors.ErrUnsupported) {
			t.Skip("guest command unsupported")
		} else if err != nil {
			t.Fatalf("Run true: %v", err)
		}
	})

	t.Run("status", func(t *testing.T) {
		m := startMachine(t)
		defer m.Kill()
		reporter, ok := m.(backend.StatusReporter)
		if !ok {
			t.Skip("status unsupported")
		}
		status, err := reporter.Status(context.Background())
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if status.State == "" {
			t.Fatal("Status state is empty")
		}
	})

	t.Run("resize_memory", func(t *testing.T) {
		m := startMachine(t)
		defer m.Kill()
		resizer, ok := m.(backend.MemoryResizer)
		if !ok {
			t.Skip("memory resize unsupported")
		}
		if err := resizer.ResizeMemory(context.Background(), 256*units.Mebibyte); errors.Is(err, errors.ErrUnsupported) {
			t.Skip("memory resize unsupported by configuration")
		} else if err != nil {
			t.Fatalf("ResizeMemory: %v", err)
		}
	})
}
