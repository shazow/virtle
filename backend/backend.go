// Package backend defines the implementer contract for virtle VM backends,
// mirroring the database/sql/driver split: consumers hold the interfaces
// declared here, implementations live in backend-named subpackages
// (backend/qemu today). Optional functionality is declared as standalone
// capability interfaces discovered by type assertion, the way driver.Conn
// implementations opt into driver.ConnBeginTx: capabilities of a running
// VM (Suspender, MemoryResizer, DeviceAttacher, ConsoleProvider) are
// asserted on the Instance, and the one capability that creates instances
// (Resumer) is asserted on the Backend.
//
// There is deliberately no default backend: this package cannot import its
// implementations without a cycle, so consumers always name their backend
// explicitly (&qemu.Backend{...}).
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
//
// Exit is observable the way a context.Context is: Done is selectable,
// Err reports the outcome once Done is closed, and Wait is the blocking
// sugar over both. Teardown follows net/http's Server: Shutdown is the
// graceful stop, Kill the hard one, and both release the instance's
// runtime state.
//
//	select {
//	case <-inst.Done():
//		err := inst.Err()
//	case <-sig:
//		inst.Shutdown(ctx)
//	}
type Instance interface {
	// Done returns a channel closed once the VM has exited and the
	// instance's runtime state is released.
	Done() <-chan struct{}
	// Err returns the exit error, valid once Done is closed: nil for a
	// clean exit. Before Done is closed it returns nil.
	Err() error
	// Wait blocks until Done is closed (or ctx is done) and returns Err,
	// or ctx's error if ctx ended first.
	Wait(ctx context.Context) error
	// Kill stops the VM immediately. It is always available.
	Kill() error
	// Shutdown stops the VM gracefully: the guest is asked to power down
	// when remote control is available, and the backend escalates to a
	// hard stop when the guest does not exit or ctx expires. Shutdown is
	// safe to call more than once and after the VM has exited.
	Shutdown(ctx context.Context) error

	// RemoteControl returns guest control for this instance, wired up by
	// the backend, or an error wrapping errors.ErrUnsupported when the VM
	// has no reachable guest agent. Most virtle functionality is built on
	// the expectation that this succeeds. Whether the backend wires guest
	// control eagerly at Start or lazily on first call is an implementation
	// detail behind the backend's constructor.
	RemoteControl() (vm.Guest, error)
}

// Resumer is implemented by backends that can restore an instance whose
// state a Suspender saved.
type Resumer interface {
	// Resume restores the instance whose saved state the spec's state
	// directory holds.
	Resume(ctx context.Context, spec *vm.Spec) (Instance, error)

	// StateVersion reports the backend's suspend-state version token
	// (e.g. "qemu-v1"). Saved state is stamped with it and compared
	// before restoring; only an exact match is resumable, since the
	// saved state is a backend-owned format.
	StateVersion() string
}

// Suspender is implemented by instances whose backend can save their
// running state to disk, to be restored later by the backend's Resumer.
type Suspender interface {
	// Suspend saves the instance's state to its state directory and stops
	// it. The instance is not usable afterwards.
	Suspend(ctx context.Context) error
}

// MemoryResizer is implemented by instances whose memory can be grown or
// shrunk while running (e.g. virtio-balloon).
type MemoryResizer interface {
	ResizeMemory(ctx context.Context, size units.Bytes) error
}

// DeviceAttacher is implemented by instances that can attach and detach
// devices while running. vm.Device is the sealed union of vm.Share,
// vm.Disk, and vm.Forward — typed, not `any`.
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

// Shutdown stops an instance gracefully.
//
// Deprecated: graceful shutdown is part of the Instance contract; call
// inst.Shutdown(ctx) directly. This alias will be removed in a future
// release.
func Shutdown(ctx context.Context, inst Instance) error {
	return inst.Shutdown(ctx)
}
