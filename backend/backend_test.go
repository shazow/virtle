package backend

import (
	"context"
	"errors"
	"testing"

	"github.com/shazow/virtle/vm"
)

type fakeInstance struct {
	shutdowns int
}

func (i *fakeInstance) Done() <-chan struct{}            { return nil }
func (i *fakeInstance) Err() error                       { return nil }
func (i *fakeInstance) Wait(context.Context) error       { return nil }
func (i *fakeInstance) Kill() error                      { return nil }
func (i *fakeInstance) RemoteControl() (vm.Guest, error) { return nil, errors.ErrUnsupported }
func (i *fakeInstance) Shutdown(context.Context) error {
	i.shutdowns++
	return nil
}

func TestShutdownCompatibilityAlias(t *testing.T) {
	inst := &fakeInstance{}
	if err := Shutdown(context.Background(), inst); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if inst.shutdowns != 1 {
		t.Errorf("shutdowns = %d, want 1", inst.shutdowns)
	}
}
