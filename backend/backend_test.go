package backend

import (
	"context"
	"testing"

	"github.com/shazow/virtle/vm"
)

// shutdownRecorder is the minimal Instance for testing the deprecated
// package-level Shutdown alias.
type shutdownRecorder struct {
	shutdowns int
}

func (i *shutdownRecorder) Wait(ctx context.Context) error   { return nil }
func (i *shutdownRecorder) Kill() error                      { return nil }
func (i *shutdownRecorder) RemoteControl() (vm.Guest, error) { return nil, nil }
func (i *shutdownRecorder) Shutdown(ctx context.Context) error {
	i.shutdowns++
	return nil
}

func TestShutdownDelegatesToInstance(t *testing.T) {
	inst := &shutdownRecorder{}
	if err := Shutdown(context.Background(), inst); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if inst.shutdowns != 1 {
		t.Errorf("instance shutdowns = %d, want 1", inst.shutdowns)
	}
}
