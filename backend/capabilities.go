package backend

import (
	"context"

	"github.com/shazow/virtle/units"
	"github.com/shazow/virtle/vm"
)

// Capability interfaces are standalone rather than embedding [Backend]: the
// caller asserting one already holds the Backend, and standalone interfaces
// compose freely (interface{ backend.Backend; backend.Suspender }). This is
// the driver.Pinger / http.Flusher shape.
//
// They take the [Instance] as an argument because the capability is a property
// of the backend, matching the sql/driver framing:
//
//	if s, ok := b.(backend.Suspender); ok {
//		err = s.Suspend(ctx, inst, stateDir)
//	}

// Suspender is implemented by backends that can save a running instance's
// state to disk and later restore it.
type Suspender interface {
	// Suspend saves the instance's state under stateDir and stops it. The
	// on-disk layout belongs to the backend and is not a public format.
	Suspend(ctx context.Context, inst Instance, stateDir string) error

	// Resume starts a machine from state previously written to stateDir by
	// the same backend. The spec must describe the same machine that was
	// suspended.
	Resume(ctx context.Context, spec *vm.Spec, stateDir string) (Instance, error)
}

// MemoryResizer is implemented by backends that can grow or shrink a running
// instance's memory, over virtio-balloon or an equivalent.
type MemoryResizer interface {
	// ResizeMemory sets the memory the guest may use. The size is a target:
	// backends report what they could not honor as an error, but a guest may
	// take time to release memory it has already touched.
	ResizeMemory(ctx context.Context, inst Instance, size units.Bytes) error
}

// DeviceAttacher is implemented by backends that can attach and detach devices
// on a running instance.
type DeviceAttacher interface {
	// Attach adds dev to the running instance.
	Attach(ctx context.Context, inst Instance, dev vm.Device) error

	// Detach removes a previously attached device.
	Detach(ctx context.Context, inst Instance, dev vm.Device) error
}

// ConsoleProvider is implemented by backends that expose an instance's
// console — the serial or chardev session that exists whether or not the guest
// runs an agent, and so is the debug path of last resort.
type ConsoleProvider interface {
	// Console returns the instance's console session. A backend that can
	// only hand out one session returns an error on a second call.
	Console(ctx context.Context, inst Instance) (vm.Term, error)
}
