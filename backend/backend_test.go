package backend

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"testing"

	"github.com/shazow/virtle/vm"
)

type fakeGuest struct {
	shutdowns   int
	shutdownErr error
}

func (g *fakeGuest) Run(ctx context.Context, cmd *vm.GuestCmd) (*vm.GuestResult, error) {
	return nil, errors.ErrUnsupported
}
func (g *fakeGuest) Open(ctx context.Context, name string) (io.ReadCloser, error) {
	return nil, errors.ErrUnsupported
}
func (g *fakeGuest) Create(ctx context.Context, name string, mode fs.FileMode) (io.WriteCloser, error) {
	return nil, errors.ErrUnsupported
}
func (g *fakeGuest) Shutdown(ctx context.Context) error {
	g.shutdowns++
	return g.shutdownErr
}
func (g *fakeGuest) Close() error { return nil }

type fakeInstance struct {
	guest    *fakeGuest
	guestErr error
	waitErr  error
	kills    int
}

func (i *fakeInstance) Wait(ctx context.Context) error { return i.waitErr }
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

func TestShutdownGraceful(t *testing.T) {
	inst := &fakeInstance{guest: &fakeGuest{}}
	if err := Shutdown(context.Background(), inst); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if inst.guest.shutdowns != 1 {
		t.Errorf("guest shutdowns = %d, want 1", inst.guest.shutdowns)
	}
	if inst.kills != 0 {
		t.Errorf("kills = %d, want 0", inst.kills)
	}
}

func TestShutdownFallsBackToKillWithoutGuest(t *testing.T) {
	inst := &fakeInstance{guestErr: errors.ErrUnsupported}
	if err := Shutdown(context.Background(), inst); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if inst.kills != 1 {
		t.Errorf("kills = %d, want 1", inst.kills)
	}
}

func TestShutdownFallsBackToKillOnGuestError(t *testing.T) {
	inst := &fakeInstance{guest: &fakeGuest{shutdownErr: errors.New("agent unreachable")}}
	if err := Shutdown(context.Background(), inst); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if inst.kills != 1 {
		t.Errorf("kills = %d, want 1", inst.kills)
	}
}
