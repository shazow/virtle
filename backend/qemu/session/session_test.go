package session

import (
	"context"
	"errors"
	"testing"

	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/backend/backendtest"
	"github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/vm"
	"github.com/shazow/virtle/vm/vmtest"
)

type notifyingBackend struct {
	backendtest.Backend
	started chan backend.Machine
}

func (b *notifyingBackend) Start(ctx context.Context, spec *vm.Spec) (backend.Machine, error) {
	m, err := b.Backend.Start(ctx, spec)
	if err == nil {
		b.started <- m
	}
	return m, err
}

func TestRunShutsMachineDownWhenContextEnds(t *testing.T) {
	g := &vmtest.Guest{}
	b := &notifyingBackend{Backend: backendtest.Backend{Guest: g}, started: make(chan backend.Machine, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, b, &vm.Spec{}, &manifest.Manifest{}, Options{Resume: "no"})
	}()
	<-b.started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
	if got := g.Shutdowns(); got != 1 {
		t.Fatalf("guest shutdowns = %d, want 1", got)
	}
}

func TestRunRejectsUnknownResumeModeBeforeStart(t *testing.T) {
	b := &notifyingBackend{Backend: backendtest.Backend{}, started: make(chan backend.Machine, 1)}
	err := Run(context.Background(), b, &vm.Spec{}, &manifest.Manifest{}, Options{Resume: "sometimes"})
	if err == nil {
		t.Fatal("Run unexpectedly accepted an unknown resume mode")
	}
	select {
	case <-b.started:
		t.Fatal("Run started a machine before rejecting the resume mode")
	default:
	}
}
