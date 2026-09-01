// Package backend defines the implementer contract for virtle VM backends,
// mirroring the database/sql/driver split: consumers hold the interfaces
// declared here, implementations live in backend-named subpackages
// (backend/qemu today). Optional functionality is declared as standalone
// capability interfaces (Suspender, MemoryResizer, ...) discovered by type
// assertion, the way driver.Conn implementations opt into driver.ConnBeginTx.
//
// There is deliberately no default backend: this package cannot import its
// implementations without a cycle, so consumers always name their backend
// explicitly (for example, &qemu.Backend{}).
package backend

import (
	"context"

	"github.com/shazow/virtle/units"
	"github.com/shazow/virtle/vm"
)

// Backend starts virtual machines. Implementations live under backend/
// (backend/qemu today; backend/firecracker, an in-process libkrun
// backend, ... later).
type Backend interface {
	Start(ctx context.Context, spec *vm.Spec) (Instance, error)
}

// Instance is a virtual machine started by a Backend. It deliberately says
// nothing about processes, sockets, or protocols, so exec'd (QEMU) and
// in-process (libkrun) backends satisfy it equally.
type Instance interface {
	// Done is closed after the VM exits and its runtime resources have been
	// released. Err reports the exit error after Done is closed.
	Done() <-chan struct{}
	Err() error
	Wait(ctx context.Context) error // blocks until Done closes or ctx ends
	Kill() error                    // hard stop, always available
	// Shutdown gracefully stops the instance, falling back to Kill when the
	// graceful path fails or ctx expires.
	Shutdown(ctx context.Context) error

	// RemoteControl returns guest control for this instance, wired up by
	// the backend, or an error wrapping errors.ErrUnsupported when the VM
	// has no reachable guest agent. Most virtle functionality is built on
	// the expectation that this succeeds. Whether the backend wires guest
	// control eagerly at Start or lazily on first call is an implementation
	// detail behind the backend's constructor.
	RemoteControl() (vm.Guest, error)
}

// Suspender is implemented by live instances that can save their state to
// the state directory selected when they were started.
type Suspender interface {
	Suspend(ctx context.Context) error
}

// Resumer is implemented by backends that can restore a suspended instance.
type Resumer interface {
	Resume(ctx context.Context, spec *vm.Spec) (Instance, error)

	// StateVersion reports the backend's suspend-state version token
	// (e.g. "qemu-v1"). Saved state is stamped with it and compared
	// before restoring; only an exact match is resumable, since the
	// saved state is a backend-owned format.
	StateVersion() string
}

// MemoryResizer is implemented by instances that can grow or shrink their
// memory (e.g. through virtio-balloon).
type MemoryResizer interface {
	ResizeMemory(ctx context.Context, size units.Bytes) error
}

// DeviceAttacher is implemented by instances that can attach and detach
// devices. vm.Device is the sealed union of
// vm.Share, vm.Disk, and vm.Forward — typed, not `any`.
type DeviceAttacher interface {
	Attach(ctx context.Context, dev vm.Device) error
	Detach(ctx context.Context, dev vm.Device) error
}

// ConsoleProvider is implemented by instances whose backend exposes a
// serial/chardev console — the no-daemon debug path. The returned Term
// may lack resize and exit semantics (see vm.Term).
type ConsoleProvider interface {
	Console(ctx context.Context) (vm.Term, error)
}

// Shutdown is a compatibility alias for Instance.Shutdown.
// Deprecated: call inst.Shutdown directly.
func Shutdown(ctx context.Context, inst Instance) error {
	return inst.Shutdown(ctx)
}
