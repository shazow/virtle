package backend_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"testing"

	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/vm"
)

func TestShutdownAsksGuestThenReleasesInstance(t *testing.T) {
	guest := &fakeGuest{}
	inst := &fakeInstance{guest: guest}

	if err := backend.Shutdown(context.Background(), inst); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if !guest.shutdown {
		t.Error("guest shutdown was not requested")
	}
	if !inst.waited {
		t.Error("Shutdown did not wait for the VM to exit")
	}
	if inst.kills != 1 {
		t.Errorf("Kill called %d times, want 1", inst.kills)
	}
}

func TestShutdownKillsWhenGuestControlIsUnsupported(t *testing.T) {
	inst := &fakeInstance{guestErr: fmt.Errorf("no guest agent: %w", errors.ErrUnsupported)}

	if err := backend.Shutdown(context.Background(), inst); err != nil {
		t.Fatalf("Shutdown() error = %v, want nil for an agentless VM", err)
	}
	if inst.waited {
		t.Error("Shutdown waited for a VM it could not ask to power down")
	}
	if inst.kills != 1 {
		t.Errorf("Kill called %d times, want 1", inst.kills)
	}
}

func TestShutdownKillsAfterFailedGuestRequest(t *testing.T) {
	shutdownErr := errors.New("guest agent is wedged")
	inst := &fakeInstance{guest: &fakeGuest{shutdownErr: shutdownErr}}

	err := backend.Shutdown(context.Background(), inst)
	if !errors.Is(err, shutdownErr) {
		t.Fatalf("Shutdown() error = %v, want %v", err, shutdownErr)
	}
	if inst.kills != 1 {
		t.Errorf("Kill called %d times, want 1", inst.kills)
	}
}

func TestShutdownReportsVMExitFailure(t *testing.T) {
	waitErr := errors.New("qemu exited with status 1")
	inst := &fakeInstance{guest: &fakeGuest{}, waitErr: waitErr}

	if err := backend.Shutdown(context.Background(), inst); !errors.Is(err, waitErr) {
		t.Fatalf("Shutdown() error = %v, want %v", err, waitErr)
	}
}

func TestShutdownIgnoresNilInstance(t *testing.T) {
	if err := backend.Shutdown(context.Background(), nil); err != nil {
		t.Fatalf("Shutdown(nil) error = %v", err)
	}
}

// The capability interfaces are satisfied by assertion, not declaration, so
// the pattern consumers use is worth an affirmative test.
func TestCapabilitiesAreDiscoveredByAssertion(t *testing.T) {
	var suspending backend.Backend = suspendingBackend{}
	if _, ok := suspending.(backend.Suspender); !ok {
		t.Error("suspendingBackend does not satisfy backend.Suspender")
	}

	var plain backend.Backend = plainBackend{}
	if _, ok := plain.(backend.Suspender); ok {
		t.Error("plainBackend unexpectedly satisfies backend.Suspender")
	}
}

type plainBackend struct{}

func (plainBackend) Start(context.Context, *vm.Spec) (backend.Instance, error) {
	return nil, errors.ErrUnsupported
}

type suspendingBackend struct{ plainBackend }

func (suspendingBackend) Suspend(context.Context, backend.Instance, string) error {
	return nil
}

func (suspendingBackend) Resume(context.Context, *vm.Spec, string) (backend.Instance, error) {
	return nil, nil
}

type fakeInstance struct {
	guest    vm.Guest
	guestErr error
	waitErr  error
	waited   bool
	kills    int
}

func (i *fakeInstance) Wait(context.Context) error {
	i.waited = true
	return i.waitErr
}

func (i *fakeInstance) Kill() error {
	i.kills++
	return nil
}

func (i *fakeInstance) RemoteControl() (vm.Guest, error) {
	if i.guestErr != nil {
		return nil, i.guestErr
	}
	return i.guest, nil
}

type fakeGuest struct {
	shutdown    bool
	shutdownErr error
}

func (g *fakeGuest) Run(context.Context, *vm.GuestCmd) (*vm.GuestResult, error) {
	return nil, errors.ErrUnsupported
}

func (g *fakeGuest) Open(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.ErrUnsupported
}

func (g *fakeGuest) Create(context.Context, string, fs.FileMode) (io.WriteCloser, error) {
	return nil, errors.ErrUnsupported
}

func (g *fakeGuest) Shutdown(context.Context) error {
	g.shutdown = true
	return g.shutdownErr
}

func (g *fakeGuest) Close() error { return nil }
