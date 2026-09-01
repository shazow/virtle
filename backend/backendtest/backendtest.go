// Package backendtest provides a reusable backend conformance test.
package backendtest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/units"
	"github.com/shazow/virtle/vm"
)

const operationTimeout = 10 * time.Second

// TestBackend exercises the backend and instance contracts. newBackend should
// return a backend configured so an empty Spec launches a small test VM.
func TestBackend(t *testing.T, newBackend func(t *testing.T) backend.Backend) {
	t.Helper()
	b := newBackend(t)
	if b == nil {
		t.Fatal("newBackend returned nil")
	}

	ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
	defer cancel()
	inst, err := b.Start(ctx, &vm.Spec{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if inst.Done() == nil {
		t.Fatal("Instance.Done returned nil")
	}
	if _, err := inst.RemoteControl(); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("RemoteControl: %v", err)
	}

	if resize, ok := inst.(backend.MemoryResizer); ok {
		if err := resize.ResizeMemory(ctx, units.Mebibyte); err != nil && !errors.Is(err, errors.ErrUnsupported) {
			t.Errorf("ResizeMemory: %v", err)
		}
	}
	if devices, ok := inst.(backend.DeviceAttacher); ok {
		dev := vm.Forward{HostAddr: ":0", GuestAddr: ":1"}
		if err := devices.Attach(ctx, dev); err == nil {
			if err := devices.Detach(ctx, dev); err != nil {
				t.Errorf("Detach: %v", err)
			}
		} else if !errors.Is(err, errors.ErrUnsupported) {
			t.Errorf("Attach: %v", err)
		}
	}

	if suspender, ok := inst.(backend.Suspender); ok {
		if err := suspender.Suspend(ctx); err == nil {
			waitDone(t, ctx, inst)
			resumer, ok := b.(backend.Resumer)
			if !ok {
				t.Fatal("instance supports Suspend but backend does not support Resume")
			}
			resumed, err := resumer.Resume(ctx, &vm.Spec{})
			if err != nil {
				t.Fatalf("Resume: %v", err)
			}
			if err := resumed.Kill(); err != nil {
				t.Errorf("Kill resumed instance: %v", err)
			}
			waitDone(t, ctx, resumed)
			return
		} else if !errors.Is(err, errors.ErrUnsupported) {
			t.Errorf("Suspend: %v", err)
		}
	}

	if err := inst.Kill(); err != nil {
		t.Errorf("Kill: %v", err)
	}
	waitDone(t, ctx, inst)
}

func waitDone(t *testing.T, ctx context.Context, inst backend.Instance) {
	t.Helper()
	select {
	case <-inst.Done():
		_ = inst.Err()
		_ = inst.Wait(ctx)
	case <-ctx.Done():
		t.Fatalf("waiting for Instance.Done: %v", context.Cause(ctx))
	}
}
