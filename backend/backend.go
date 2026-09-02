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
	"time"

	"github.com/shazow/virtle/units"
	"github.com/shazow/virtle/vm"
)

// Backend starts virtual machines. Implementations live under backend/
// (backend/qemu today; backend/firecracker, an in-process libkrun
// backend, ... later).
type Backend interface {
	Start(ctx context.Context, spec *vm.Spec) (Machine, error)
}

// Machine is a virtual machine started by a Backend. It deliberately says
// nothing about processes, sockets, or protocols, so exec'd (QEMU) and
// in-process (libkrun) backends satisfy it equally.
type Machine interface {
	// Done closes after the machine exits and its runtime state is released.
	Done() <-chan struct{}
	// Err reports the exit result after Done closes.
	Err() error
	Wait(ctx context.Context) error // blocks until the VM exits
	Kill() error                    // hard stop, always available
	// Shutdown gracefully stops the machine, falling back to Kill when ctx
	// expires. Implementations must make repeated calls safe.
	Shutdown(ctx context.Context) error

	// RemoteControl returns guest control for this machine, wired up by
	// the backend, or an error wrapping errors.ErrUnsupported when the VM
	// has no reachable guest agent. Most virtle functionality is built on
	// the expectation that this succeeds. Whether the backend wires guest
	// control eagerly at Start or lazily on first call is an implementation
	// detail behind the backend's constructor.
	RemoteControl() (vm.Guest, error)
}

// State is a machine lifecycle state.
type State string

const (
	StateStarting   State = "starting"
	StateReady      State = "ready"
	StateSuspending State = "suspending"
	StateSuspended  State = "suspended"
	StateStopping   State = "stopping"
	StateStopped    State = "stopped"
)

// Status reports machine lifecycle state and host-side connection details.
// JSON field names are the stable control-socket wire names.
type Status struct {
	State State        `json:"state"`
	CID   int          `json:"cid"`
	PID   int          `json:"pid,omitempty"`
	Paths StatusPaths  `json:"paths"`
	Stats RuntimeStats `json:"stats"`
}

// StatusPaths are host-side sockets associated with a machine.
type StatusPaths struct {
	ControlSocket      string `json:"controlSocket"`
	MonitorSocket      string `json:"qmpSocket"`
	GuestControlSocket string `json:"guestAgentSocket,omitempty"`
	ReadySocket        string `json:"sshReadySocket,omitempty"`
}

// RuntimeStats reports lifecycle timing captured during launch and teardown.
type RuntimeStats struct {
	StartedAt        time.Time `json:"startedAt,omitempty"`
	BootStartedAt    time.Time `json:"bootStartedAt,omitempty"`
	MonitorReadyAt   time.Time `json:"qmpReadyAt,omitempty"`
	FilesReadyAt     time.Time `json:"filesReadyAt,omitempty"`
	ReadyAt          time.Time `json:"sshReadyAt,omitempty"`
	SessionStartedAt time.Time `json:"sshStartedAt,omitempty"`
	CompletedAt      time.Time `json:"completedAt,omitempty"`
	SessionAttempts  int       `json:"sshAttempts,omitempty"`
	StartedToBoot    string    `json:"startedToBoot,omitempty"`
	BootToMonitor    string    `json:"bootToQMP,omitempty"`
	FilesToReady     string    `json:"filesToSSH,omitempty"`
	BootToCompleted  string    `json:"bootToCompleted,omitempty"`
	Total            string    `json:"total,omitempty"`
}

// StatusReporter is implemented by machines that report runtime status.
type StatusReporter interface {
	Status(ctx context.Context) (Status, error)
}

// Suspender is implemented by machines that can save their running state to
// their state directory and stop.
type Suspender interface {
	Suspend(ctx context.Context) error
}

// Resumer is implemented by backends that can restore a suspended machine.
type Resumer interface {
	Resume(ctx context.Context, spec *vm.Spec) (Machine, error)

	// StateVersion reports the backend's suspend-state version token
	// (e.g. "qemu-v1"). Saved state is stamped with it and compared
	// before restoring; only an exact match is resumable, since the
	// saved state is a backend-owned format.
	StateVersion() string
}

// MemoryResizer is implemented by machines that can grow or shrink their
// memory (e.g. virtio-balloon).
type MemoryResizer interface {
	ResizeMemory(ctx context.Context, size units.Bytes) error
}

// DeviceAttacher is implemented by machines that can attach and detach
// devices. vm.Device is the sealed union of
// vm.Share, vm.Disk, and vm.Forward — typed, not `any`.
type DeviceAttacher interface {
	Attach(ctx context.Context, dev vm.Device) error
	Detach(ctx context.Context, dev vm.Device) error
}

// ConsoleProvider is implemented by machines whose backend exposes a
// serial/chardev console — the no-daemon debug path. The returned Term
// may lack resize and exit semantics (see vm.Term).
type ConsoleProvider interface {
	Console(ctx context.Context) (vm.Term, error)
}

// Shutdown stops a machine gracefully. The graceful guest shutdown is
// attempted only when remote control is available (RemoteControl
// succeeds); instances without it — and instances whose guest is
// unreachable or whose context expires — are stopped with Kill.
// Deprecated: call Machine.Shutdown directly.
func Shutdown(ctx context.Context, m Machine) error {
	return m.Shutdown(ctx)
}
