// Package backend is the contract virtual machine backends implement, and the
// seam consumers hold a running machine through.
//
// It plays the role database/sql/driver plays for database/sql: [Backend] and
// [Instance] are cut to the minimum every backend can provide, and anything
// optional is a separate capability interface discovered by type assertion —
// so adding functionality never breaks an implementer. Backends live in
// subpackages (backend/qemu today) and are always named explicitly by the
// consumer; this package deliberately cannot reach them, which is why there is
// no default backend to accidentally depend on.
//
// This package is experimental. The module is untagged v0 and the API may
// change until it settles.
package backend

import (
	"context"

	"github.com/shazow/virtle/vm"
)

// Backend starts virtual machines.
//
// Implementations are obtained from their own package's constructor —
// qemu.BackendWithQGA, say — which is where backend-specific configuration and
// the choice of guest-control implementation live.
type Backend interface {
	// Start boots a machine described by spec and returns once it is running
	// and its guest control, if any, is reachable. The returned Instance
	// owns host resources until it is stopped, so callers pair Start with
	// [Shutdown] or Instance.Kill.
	Start(ctx context.Context, spec *vm.Spec) (Instance, error)
}

// Instance is a virtual machine started by a [Backend].
//
// It deliberately says nothing about processes, sockets, or protocols, so an
// exec'd backend such as QEMU and an in-process one such as libkrun satisfy it
// equally.
type Instance interface {
	// Wait blocks until the VM exits, or until ctx ends. It reports the VM's
	// own failure, if any; a VM that exits cleanly returns nil.
	Wait(ctx context.Context) error

	// Kill stops the VM immediately and releases the host resources it
	// holds. It is always available and safe to call more than once — a
	// second call on a stopped instance is a no-op.
	Kill() error

	// RemoteControl returns guest control for this instance, wired up by the
	// backend, or an error wrapping errors.ErrUnsupported when the VM has no
	// reachable guest agent. Most of virtle is built on the expectation that
	// it succeeds. Whether the backend wires guest control eagerly at Start
	// or lazily here is an implementation detail behind its constructor.
	//
	// The returned Guest belongs to the instance; it is closed when the
	// instance stops and callers do not close it themselves.
	RemoteControl() (vm.Guest, error)
}
