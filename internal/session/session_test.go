package session

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/vm"
)

type startFunc func(context.Context, *vm.Spec) (backend.Machine, error)

func (f startFunc) Start(ctx context.Context, s *vm.Spec) (backend.Machine, error) { return f(ctx, s) }

func TestBackendNeutralSession(t *testing.T) {
	called := false
	want := errors.New("start result")
	b := startFunc(func(context.Context, *vm.Spec) (backend.Machine, error) { called = true; return nil, want })
	if err := Run(t.Context(), b, &vm.Spec{}, &manifest.Manifest{}, Options{}); !errors.Is(err, want) || !called {
		t.Fatalf("called %v, err %v", called, err)
	}
}

type completedMachine struct {
	backend.Machine
	done chan struct{}
}

func (m completedMachine) Done() <-chan struct{} { return m.done }
func (m completedMachine) Err() error            { return nil }

func TestSessionReportsStartup(t *testing.T) {
	m := completedMachine{done: make(chan struct{})}
	close(m.done)
	b := startFunc(func(context.Context, *vm.Spec) (backend.Machine, error) { return m, nil })
	var logs bytes.Buffer
	err := Run(t.Context(), b, &vm.Spec{}, &manifest.Manifest{}, Options{Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "vm startup complete") {
		t.Fatalf("startup log: %s", logs.String())
	}
}

type ownedMachine struct {
	backend.Machine
	startCtx context.Context
	done     chan struct{}
	t        *testing.T
}

func (m *ownedMachine) Done() <-chan struct{} { return m.done }
func (m *ownedMachine) Shutdown(context.Context) error {
	if err := m.startCtx.Err(); err != nil {
		m.t.Error("machine lifetime canceled before graceful shutdown:", err)
	}
	close(m.done)
	return nil
}

func TestSessionOwnsGracefulShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	m := &ownedMachine{done: make(chan struct{}), t: t}
	b := startFunc(func(startCtx context.Context, _ *vm.Spec) (backend.Machine, error) {
		m.startCtx = startCtx
		return m, nil
	})
	// Trigger cancellation only once startup has completed and foreground owns
	// the VM. Status is queried by waitReady before the foreground select.
	m.Machine = &cancelOnStatus{cancel: cancel}
	err := Run(ctx, b, &vm.Spec{}, &manifest.Manifest{}, Options{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

type cancelOnStatus struct {
	backend.Machine
	cancel context.CancelFunc
}

func (m *cancelOnStatus) Status(context.Context) (backend.Status, error) {
	m.cancel()
	return backend.Status{}, nil
}
func (m *ownedMachine) Status(ctx context.Context) (backend.Status, error) {
	return m.Machine.(backend.StatusReporter).Status(ctx)
}

func TestSessionCapabilitiesBeforeStart(t *testing.T) {
	for _, opts := range []Options{{SSH: true}, {Resume: "force"}, {Resume: "invalid"}} {
		b := startFunc(func(context.Context, *vm.Spec) (backend.Machine, error) { t.Fatal("unexpected start"); return nil, nil })
		if err := Run(t.Context(), b, &vm.Spec{}, &manifest.Manifest{}, opts); err == nil {
			t.Fatal("expected capability error")
		}
	}
}
