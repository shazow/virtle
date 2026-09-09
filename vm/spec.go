// Package vm holds the consumer-facing types for describing and controlling
// virtual machines: the neutral Spec a backend launches, and the Guest
// interface for operating inside a running VM.
package vm

import (
	"io"
	"io/fs"

	"github.com/shazow/virtle/units"
)

// Spec describes a virtual machine, independent of backend. The zero value
// plus a boot source is launchable; backends apply defaults. A Spec holds
// no live resources and is reusable across Start and Resume calls, with
// one caveat: Files content readers are consumed by Start (see File).
type Spec struct {
	CPUs   int         // zero selects the host CPU count (within the backend's limit)
	Memory units.Bytes // zero selects the backend's default (e.g. qemu.DefaultMemory)
	Kernel Kernel      // direct kernel boot (microVM style); zero value: none
	Shares []Share     // host dirs shared into the guest (virtio-fs or similar)
	Disks  []Disk      // block devices / volume images
	Ports  []Forward   // host<->guest port forwards
	Files  []File      // small files placed in the guest before workload start

	// Dir is the host working directory: relative Kernel, Disk, and Share
	// paths resolve against it (a disk image created from Disk.Size is
	// written at its Path), and the machine's runtime state (lock, sockets,
	// suspend state) lives in its .virtle subdirectory, as for a manifest's
	// working_dir. Empty means the process
	// working directory, as for exec.Cmd.Dir, with runtime state in a private
	// temporary directory that is removed when the machine exits; set Dir to
	// keep state across runs, which Suspend and Resume require.
	Dir string
}

// Kernel configures direct kernel boot (microVM style).
type Kernel struct {
	Path    string // kernel image (vmlinuz)
	Initrd  string // initial ramdisk; optional
	Cmdline string // kernel command line; optional
}

// Share is a host directory shared into the guest (virtio-fs or similar).
type Share struct {
	Tag       string // mount tag visible in the guest
	HostPath  string
	GuestPath string
	ReadOnly  bool
}

// Disk is a block device or volume image attached to the guest.
type Disk struct {
	ReadOnly  bool        // attach without allowing guest writes
	Path      string      // host image path
	GuestPath string      // guest mount point; "/" makes this the root device (virtle passes root=); other paths need a guest agent and fail Start with errors.ErrUnsupported until one exists
	Format    string      // image format (e.g. "qcow2", "raw"); backend default when empty
	Size      units.Bytes // created at this size if the image is absent
}

// Proto is a port-forward transport protocol.
type Proto string

const (
	TCP Proto = "tcp"
	UDP Proto = "udp"
)

// Forward is a host<->guest port forward.
type Forward struct {
	HostAddr  string // "host:port" or ":port"; hostnames are allowed
	GuestAddr string // "host:port" or ":port"; hostnames are allowed
	Proto     Proto  // zero value means TCP
}

// File is a small file placed in the guest before the workload starts;
// large trees go through GuestWithCopy after boot. Content is consumed by
// Start — refresh it (e.g. a fresh bytes.NewReader) before reusing the
// Spec.
type File struct {
	GuestPath string
	Content   io.Reader
	Mode      fs.FileMode
}

// Device is a device description that can be attached to a running
// machine: Share, Disk, or Forward. It is sealed (unexported method) so
// backend.DeviceAttacher stays typed rather than accepting `any`.
type Device interface{ device() }

func (Share) device()   {}
func (Disk) device()    {}
func (Forward) device() {}
