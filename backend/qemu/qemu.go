// Package qemu implements a virtle backend that launches virtual machines
// with QEMU. Backend.RemoteControl selects the guest-control transport
// wired into Machine.RemoteControl: QGA (the QEMU Guest Agent, equivalent
// to the virtle CLI today) now, a virtle-native guest daemon transport
// later.
//
// # Resource limits
//
// The backend bounds data buffered across guest and local-control trust
// boundaries. Run
//
//	go doc github.com/shazow/virtle/backend/qemu/limits
//
// to inspect the enforced defaults and the error returned when a guest
// operation crosses one. Control clients receive an RPC error with code
// resource_limit.
package qemu

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/backend/qemu/internal/vmm"
	imanifest "github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/units"
	"github.com/shazow/virtle/vm"
)

// DefaultMemory is the guest memory size used when vm.Spec.Memory is zero.
const DefaultMemory = 2048 * units.Mebibyte

// Console selects how the guest serial console is wired.
type Console string

const (
	ConsoleOff         Console = "off"     // no serial console (default)
	ConsolePrint       Console = "print"   // guest console output printed to the host
	ConsoleInteractive Console = "console" // interactive console on the host terminal
)

// Accel selects the QEMU accelerator.
type Accel string

const (
	AccelAuto Accel = ""    // KVM when available, otherwise TCG
	AccelKVM  Accel = "kvm" // require KVM
	AccelTCG  Accel = "tcg" // software emulation
)

// Backend starts QEMU virtual machines. The zero value works. Fields must not
// be modified after Start or Resume is first called.
type Backend struct {
	Binary         string            // default: qemu-system-<arch>
	MachineType    string            // QEMU machine type (-machine type=); default: microvm
	MachineOptions map[string]string // extra machine options
	CPUModel       string            // QEMU CPU model; default derived per host
	Accel          Accel             // acceleration; default AccelAuto
	ExtraArgs      []string          // passthrough QEMU arguments
	Console        Console
	Seccomp        bool   // enable QEMU seccomp sandboxing
	Balloon        bool   // attach a virtio-balloon device (required for ResizeMemory)
	HostName       string // guest-visible name; default: "virtle"

	// HotplugPorts reserves PCIe hotplug ports at boot so devices can be
	// attached later via backend.DeviceAttacher. Reserving ports forces
	// the PCI transport (as any hotplug configuration does).
	HotplugPorts int

	// RemoteControl selects the guest-control transport wired into
	// Machine.RemoteControl, declaring what the VM image runs. Nil
	// declares an image with no control agent: guest-dependent features
	// (guest file writes, workspace mounts) are disabled, RemoteControl
	// reports errors.ErrUnsupported, and teardown skips the graceful
	// guest shutdown attempt.
	RemoteControl RemoteControl

	// Logger receives manifest, VM lifecycle, SSH, and balloon logs. The
	// default discards logs.
	Logger *slog.Logger

	// ConsoleOutput receives explicitly requested console output. The default
	// is os.Stderr.
	ConsoleOutput io.Writer

	doc *imanifest.Document // transitional manifest.Load overlay; removed with consolidation
}

// RemoteControl is a guest-control transport for Backend.RemoteControl.
// It is sealed (unexported method): QGA today, the virtle-native guest
// daemon later. Each transport carries its own knobs.
type RemoteControl interface{ remoteControl() }

// QGA is the qemu-guest-agent transport: the guest image runs
// qemu-guest-agent on the default virtio-serial channel
// (org.qemu.guest_agent.0), as with the virtle CLI today. The zero value
// works.
type QGA struct {
	// SocketPath overrides where the host-side guest-agent socket is
	// placed, relative to the state dir unless absolute.
	// Default: "qga.sock".
	SocketPath string
}

func (QGA) remoteControl() {}

func (b *Backend) logger() *slog.Logger {
	if b.Logger != nil {
		return b.Logger
	}
	return slog.New(slog.DiscardHandler)
}

func (b *Backend) consoleOutput() io.Writer {
	if b.ConsoleOutput != nil {
		return b.ConsoleOutput
	}
	return os.Stderr
}

func (b *Backend) hasRemoteControl() bool { return b.RemoteControl != nil }

// StateVersion implements backend.Resumer: it reports the suspend-state
// version this backend's machinery stamps on saves and compares on
// resume. Only an exact match is resumable, since the saved VM state is a
// QEMU migration stream.
func (b *Backend) StateVersion() string { return vmm.StateVersion }

// NewBackendFromDocument is the bridge for the public manifest package:
// the returned backend starts from the loaded document, preserving
// manifest sections that have no vm.Spec equivalent, and overlays the Spec
// passed to Start on top. The document type is internal, so this is not
// callable (and not supported) outside the module.
func NewBackendFromDocument(doc imanifest.Document, b Backend) backend.Backend {
	// Manifests describe agent-equipped guests today, matching the CLI.
	if b.RemoteControl == nil {
		b.RemoteControl = QGA{}
	}
	b.doc = &doc
	return &b
}

func (b *Backend) Start(ctx context.Context, spec *vm.Spec) (backend.Machine, error) {
	return b.start(ctx, spec, vmm.ResumeModeNo)
}

// resolveSpec lowers the spec (plus any base document) through the manifest
// resolution pipeline.
func (b *Backend) resolveSpec(spec *vm.Spec, logger *slog.Logger) (*imanifest.Manifest, error) {
	doc, err := specDocument(spec, b, b.doc)
	if err != nil {
		return nil, err
	}
	mf, err := doc.ManifestWithOptions(imanifest.ResolveOptions{Logger: logger.With("package", "manifest")})
	if err != nil {
		return nil, fmt.Errorf("resolve vm spec: %w", err)
	}
	return mf, nil
}

func (b *Backend) start(ctx context.Context, spec *vm.Spec, resume vmm.ResumeMode) (backend.Machine, error) {
	logger := b.logger()
	mf, err := b.resolveSpec(spec, logger)
	if err != nil {
		return nil, err
	}
	handle, err := vmm.StartVM(ctx, mf, vmm.StartOptions{
		Resume:           resume,
		HasRemoteControl: b.hasRemoteControl(),
	}, vmm.Config{
		Logger:        logger,
		ConsoleOutput: b.consoleOutput(),
	})
	if err != nil {
		return nil, err
	}
	return &Machine{vm: handle, hasRemoteControl: b.hasRemoteControl()}, nil
}

// Machine is a QEMU virtual machine started by Backend.
type Machine struct {
	vm               *vmm.VM
	hasRemoteControl bool
}

func (m *Machine) Wait(ctx context.Context) error { return m.vm.Wait(ctx) }

func (m *Machine) Kill() error { return m.vm.Kill() }

func (m *Machine) RemoteControl() (vm.Guest, error) {
	if !m.hasRemoteControl {
		return nil, fmt.Errorf("vm has no guest control agent: %w", errors.ErrUnsupported)
	}
	return &qgaGuest{vm: m.vm}, nil
}

// Shutdown gracefully tears the machine down: guest shutdown, then QMP
// quit, then signals, then runtime cleanup. The graceful guest shutdown
// is attempted only when the backend was configured with remote control;
// without it teardown goes straight to QMP quit. Wait and Kill already
// release runtime state; Shutdown covers callers abandoning a VM without
// waiting.
func (m *Machine) Shutdown(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return errors.Join(err, m.Kill())
	}
	return m.vm.Close()
}

// Suspend implements backend.Suspender: it saves the running machine's state
// via QMP migration to its state directory and stops the VM.
func (m *Machine) Suspend(ctx context.Context) error {
	return m.vm.Suspend(ctx)
}

// Resume implements backend.Resumer: it restores a previously suspended
// machine. The spec must resolve to the state directory containing the save.
func (b *Backend) Resume(ctx context.Context, spec *vm.Spec) (backend.Machine, error) {
	return b.start(ctx, spec, vmm.ResumeModeForce)
}

// ResizeMemory implements backend.MemoryResizer via virtio-balloon. The
// backend must be configured with Backend.Balloon (or a manifest balloon
// device).
func (m *Machine) ResizeMemory(ctx context.Context, size units.Bytes) error {
	return m.vm.ResizeMemory(ctx, size.Int64())
}

// Attach implements backend.DeviceAttacher over QMP hotplug. The machine
// must have PCIe hotplug ports reserved at Start: set Backend.HotplugPorts
// (or a manifest [hotplug] section / hotplug.ports for manifest.Load
// backends).
func (m *Machine) Attach(ctx context.Context, dev vm.Device) error {
	hdev, err := hotplugDevice(m.vm, dev)
	if err != nil {
		return err
	}
	return m.vm.AttachHotplugDevice(ctx, hdev)
}

// Detach implements backend.DeviceAttacher; see Attach.
func (m *Machine) Detach(ctx context.Context, dev vm.Device) error {
	hdev, err := hotplugDevice(m.vm, dev)
	if err != nil {
		return err
	}
	return m.vm.DetachHotplugDevice(ctx, hdev.ID)
}

var (
	_ backend.Backend        = (*Backend)(nil)
	_ backend.Resumer        = (*Backend)(nil)
	_ backend.Suspender      = (*Machine)(nil)
	_ backend.MemoryResizer  = (*Machine)(nil)
	_ backend.DeviceAttacher = (*Machine)(nil)
)

type hotplugResolver interface {
	ResolveHotplugMount(imanifest.MountEntry) (imanifest.HotplugDevice, error)
	ResolveHotplugNetwork(imanifest.NetworkInput) (imanifest.HotplugDevice, error)
}

// hotplugDevice maps the sealed vm.Device union onto manifest inputs, then
// resolves them into an executable internal hotplug device description.
func hotplugDevice(resolver hotplugResolver, dev vm.Device) (imanifest.HotplugDevice, error) {
	switch d := dev.(type) {
	case vm.Share:
		if d.Tag == "" {
			return imanifest.HotplugDevice{}, fmt.Errorf("share device: Tag is required")
		}
		return resolver.ResolveHotplugMount(imanifest.VirtioFSMountInput{
			Type: imanifest.MountTypeVirtioFS,
			MountInput: imanifest.MountInput{
				Tag:        d.Tag,
				SourcePath: d.HostPath,
				ReadOnly:   d.ReadOnly,
			},
			Target: d.GuestPath,
		})
	case vm.Disk:
		if d.Path == "" {
			return imanifest.HotplugDevice{}, fmt.Errorf("disk device: Path is required")
		}
		id := deviceID("disk", d.Path)
		return resolver.ResolveHotplugMount(imanifest.ImageMountInput{
			Type:       imanifest.MountTypeImage,
			SourcePath: d.Path,
			Image:      imanifest.ImageInput{Format: d.Format, Serial: &id},
		})
	case vm.Forward:
		proto := d.Proto
		if proto == "" {
			proto = vm.TCP
		}
		identity := string(proto) + "\x00" + d.HostAddr + "\x00" + d.GuestAddr
		return resolver.ResolveHotplugNetwork(imanifest.NetworkInput{
			ID:  deviceID("fwd", identity),
			MAC: deviceMAC(identity),
			Forward: []imanifest.ForwardPort{{
				Proto: string(proto),
				From:  "host",
				Host:  d.HostAddr,
				Guest: d.GuestAddr,
			}},
		})
	default:
		return imanifest.HotplugDevice{}, fmt.Errorf("unsupported device type %T", dev)
	}
}

// deviceID derives a stable, collision-resistant QEMU ID without embedding
// caller-controlled path or address syntax.
func deviceID(kind, identity string) string {
	digest := sha256.Sum256([]byte(identity))
	return fmt.Sprintf("%s-%x", kind, digest[:8])
}

func deviceMAC(identity string) string {
	digest := sha256.Sum256([]byte(identity))
	return fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x", digest[0], digest[1], digest[2], digest[3], digest[4])
}
