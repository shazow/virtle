// Package backend defines the implementer contract for virtle VM backends,
// mirroring the database/sql/driver split: consumers hold the interfaces
// declared here, implementations live in backend-named subpackages
// (backend/qemu today). Optional functionality is declared as standalone
// capability interfaces (Suspender, MemoryResizer, ...) discovered by type
// assertion, the way driver.Conn implementations opt into driver.ConnBeginTx.
//
// There is deliberately no default backend: this package cannot import its
// implementations without a cycle, so consumers always name their backend
// explicitly (qemu.New(...)).
package backend

import (
	"context"
	"errors"
	"log/slog"

	"github.com/shazow/virtle/units"
	"github.com/shazow/virtle/vm"
)

// Backend starts virtual machines. Implementations live under backend/
// (backend/qemu today; backend/firecracker, an in-process libkrun
// backend, ... later).
type Backend interface {
	// SetLogger sets the logger used by subsequently started instances. A nil
	// logger discards logs. Implementations must permit concurrent calls.
	SetLogger(*slog.Logger)
	Start(ctx context.Context, spec *vm.Spec) (Instance, error)
}

// Instance is a virtual machine started by a Backend. It deliberately says
// nothing about processes, sockets, or protocols, so exec'd (QEMU) and
// in-process (libkrun) backends satisfy it equally.
type Instance interface {
	Wait(ctx context.Context) error // blocks until the VM exits
	Kill() error                    // hard stop, always available

	// RemoteControl returns guest control for this instance, wired up by
	// the backend, or an error wrapping errors.ErrUnsupported when the VM
	// has no reachable guest agent. Most virtle functionality is built on
	// the expectation that this succeeds. Whether the backend wires guest
	// control eagerly at Start or lazily on first call is an implementation
	// detail behind the backend's constructor.
	RemoteControl() (vm.Guest, error)
}

// Suspender is implemented by backends that can save a running instance's
// state to disk and later restore it.
type Suspender interface {
	Suspend(ctx context.Context, inst Instance, stateDir string) error
	Resume(ctx context.Context, spec *vm.Spec, stateDir string) (Instance, error)

	// StateVersion reports the backend's suspend-state version token
	// (e.g. "qemu-v1"). Saved state is stamped with it and compared
	// before restoring; only an exact match is resumable, since the
	// saved state is a backend-owned format.
	StateVersion() string
}

// MemoryResizer is implemented by backends that can grow or shrink a
// running instance's memory (e.g. virtio-balloon).
type MemoryResizer interface {
	ResizeMemory(ctx context.Context, inst Instance, size units.Bytes) error
}

// DeviceAttacher is implemented by backends that can attach and detach
// devices on a running instance. vm.Device is the sealed union of
// vm.Share, vm.Disk, and vm.Forward — typed, not `any`.
type DeviceAttacher interface {
	Attach(ctx context.Context, inst Instance, dev vm.Device) error
	Detach(ctx context.Context, inst Instance, dev vm.Device) error
}

// ConsoleProvider is implemented by instances whose backend exposes a
// serial/chardev console — the no-daemon debug path. The returned Term
// may lack resize and exit semantics (see vm.Term).
type ConsoleProvider interface {
	Console(ctx context.Context) (vm.Term, error)
}

// Shutdown stops an instance gracefully. The graceful guest shutdown is
// attempted only when remote control is available (RemoteControl
// succeeds); instances without it — and instances whose guest is
// unreachable or whose context expires — are stopped with Kill.
func Shutdown(ctx context.Context, inst Instance) error {
	g, err := inst.RemoteControl()
	if err != nil {
		return inst.Kill()
	}
	if err := g.Shutdown(ctx); err != nil {
		return inst.Kill()
	}
	if err := inst.Wait(ctx); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return inst.Kill()
		}
		return err
	}
	return nil
}
