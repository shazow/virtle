package vm

import (
	"io"
	"io/fs"

	"github.com/shazow/virtle/units"
)

// Spec describes a virtual machine, independent of backend. The zero value
// plus a boot source is launchable; backends apply their own defaults for
// anything left unset. A Spec holds no live resources and is reusable across
// Start and Resume calls, with one caveat: the content readers in Files are
// consumed by Start (see [File]).
type Spec struct {
	// CPUs is the number of virtual CPUs. Zero lets the backend decide.
	CPUs int

	// Memory is the guest memory size. Zero lets the backend decide.
	Memory units.Bytes

	// Kernel boots the guest directly, microVM style. The zero value boots
	// from the disks instead, for backends that support it.
	Kernel Kernel

	// Shares are host directories made visible inside the guest.
	Shares []Share

	// Disks are block devices attached to the guest.
	Disks []Disk

	// Ports are host↔guest port forwards.
	Ports []Forward

	// Files are small files placed in the guest before the workload starts.
	Files []File

	// Dir is the host directory holding runtime state for this machine —
	// sockets, locks, images created on demand. Empty derives one.
	Dir string
}

// Kernel is a direct kernel boot source.
type Kernel struct {
	// Path is the host path of the kernel image.
	Path string

	// Initrd is the host path of the initial ramdisk. May be empty.
	Initrd string

	// Cmdline holds kernel command-line parameters appended after whatever
	// the backend needs for its own wiring.
	Cmdline string
}

// IsZero reports whether k names no boot source.
func (k Kernel) IsZero() bool {
	return k == Kernel{}
}

// Share is a host directory shared into the guest, over virtio-fs or an
// equivalent the backend provides.
type Share struct {
	// Tag identifies the share to the guest, which mounts it by tag.
	Tag string

	// HostPath is the directory on the host.
	HostPath string

	// GuestPath is where the guest is expected to mount the share. Backends
	// carry it for guest-side helpers; it does not itself perform the mount.
	GuestPath string

	// ReadOnly shares the directory without write access.
	ReadOnly bool
}

func (Share) device() {}

// Disk is a block device backed by a host image.
type Disk struct {
	// Path is the host path of the image.
	Path string

	// GuestPath is where the guest is expected to mount the device.
	GuestPath string

	// Format names the image format, such as "raw" or "qcow2". Empty lets
	// the backend decide.
	Format string

	// Size creates the image at this size when it does not already exist.
	// Zero never creates.
	Size units.Bytes

	// ReadOnly attaches the device without write access.
	ReadOnly bool
}

func (Disk) device() {}

// Forward is a host↔guest port forward.
type Forward struct {
	// HostAddr is the host endpoint, in "host:port" form.
	HostAddr string

	// GuestAddr is the guest endpoint, in "host:port" form.
	GuestAddr string

	// Proto is the transport protocol. Empty means "tcp".
	Proto string

	// FromGuest reverses the direction: the guest listens and connections
	// are forwarded to the host endpoint.
	FromGuest bool
}

func (Forward) device() {}

// File is a small file placed in the guest before the workload starts. Large
// trees go through [GuestWithCopy] after boot instead.
//
// Content is read once, by Start; refresh it — a fresh bytes.NewReader, say —
// before reusing the Spec for another Start.
type File struct {
	// GuestPath is the absolute path to write in the guest.
	GuestPath string

	// Content is the file's contents.
	Content io.Reader

	// Mode is the guest file mode. Zero lets the backend decide.
	Mode fs.FileMode
}

// Device is something that can be attached to a running instance: [Share],
// [Disk], or [Forward]. The interface is sealed by an unexported method, so
// backend.DeviceAttacher stays typed rather than taking an any.
type Device interface {
	device()
}
