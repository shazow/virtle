package backend

import (
	"context"
	"errors"
	"testing"

	"github.com/shazow/virtle/vm"
)

type fakeMachine struct {
	shutdowns int
	err       error
}

func (*fakeMachine) Wait(context.Context) error       { return nil }
func (*fakeMachine) Kill() error                      { return nil }
func (*fakeMachine) RemoteControl() (vm.Guest, error) { return nil, errors.ErrUnsupported }
func (m *fakeMachine) Shutdown(context.Context) error {
	m.shutdowns++
	return m.err
}

func TestShutdownDelegatesToMachine(t *testing.T) {
	wantErr := errors.New("shutdown failed")
	m := &fakeMachine{err: wantErr}
	if err := Shutdown(context.Background(), m); !errors.Is(err, wantErr) {
		t.Fatalf("Shutdown() error = %v, want %v", err, wantErr)
	}
	if m.shutdowns != 1 {
		t.Errorf("Shutdown calls = %d, want 1", m.shutdowns)
	}
}
