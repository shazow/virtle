// Package qemu implements a virtle backend that launches virtual machines
// with QEMU. Backend is the configuration and the backend.Backend in one
// exported struct, the http.Server shape: its zero value works, defaults
// are applied at first use, and Backend.RemoteControl selects the
// guest-control transport wired into Instance.RemoteControl: QGA (the
// QEMU Guest Agent, equivalent to the virtle CLI today) now, a
// virtle-native guest daemon transport later.
//
//	b := &qemu.Backend{RemoteControl: qemu.QGA{}}
//	inst, err := b.Start(ctx, spec)
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

// Accel selects the QEMU accelerator.
type Accel string

const (
	AccelAuto Accel = ""    // KVM when available, else TCG (default)
	AccelKVM  Accel = "kvm" // require KVM
	AccelTCG  Accel = "tcg" // software emulation, even when KVM is available
)

// Console selects how the guest serial console is wired.
type Console string

const (
	ConsoleOff         Console = "off"     // no serial console (default)
	ConsolePrint       Console = "print"   // guest console output printed to the host
	ConsoleInteractive Console = "console" // interactive console on the host terminal
)

// Backend is the QEMU backend: its fields are the QEMU-only knobs, and
// the zero value works. Fields must not be modified after Start or Resume
// has been called, as with http.Server.
type Backend struct {
	Binary         string            // default: qemu-system-<arch>
	Machine        string            // QEMU machine type; default: microvm
	MachineOptions map[string]string // extra machine options
	CPUModel       string            // QEMU CPU model; default derived per host
	Accel          Accel             // accelerator; default: AccelAuto
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
	// Instance.RemoteControl, declaring what the VM image runs. Nil
	// declares an image with no control agent: guest-dependent features
	// (guest file writes, workspace mounts) are disabled, RemoteControl
	// reports errors.ErrUnsupported, and teardown skips the graceful
	// guest shutdown attempt.
	RemoteControl RemoteControl

	// Logger receives manifest, VM lifecycle, SSH, and balloon logs. Nil
	// discards logs.
	Logger *slog.Logger

	// ConsoleOutput receives explicitly requested console output. Nil
	// means os.Stderr.
	ConsoleOutput io.Writer

	// doc is the loaded manifest document for manifest.Load-configured
	// backends; nil for backends constructed directly.
	doc *imanifest.Document
}

var (
	_ backend.Backend        = (*Backend)(nil)
	_ backend.Resumer        = (*Backend)(nil)
	_ backend.Suspender      = (*Instance)(nil)
	_ backend.MemoryResizer  = (*Instance)(nil)
	_ backend.DeviceAttacher = (*Instance)(nil)
)

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
	if b.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return b.Logger
}

func (b *Backend) consoleOutput() io.Writer {
	if b.ConsoleOutput == nil {
		return os.Stderr
	}
	return b.ConsoleOutput
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
// passed to Start on top. Manifests describe agent-equipped guests today,
// matching the CLI, so the backend's RemoteControl is QGA. The document
// type is internal, so this is not callable (and not supported) outside
// the module.
func NewBackendFromDocument(doc imanifest.Document) *Backend {
	return &Backend{RemoteControl: QGA{}, doc: &doc}
}

// Start launches a VM from spec. It implements backend.Backend.
func (b *Backend) Start(ctx context.Context, spec *vm.Spec) (backend.Instance, error) {
	return b.start(ctx, spec, vmm.ResumeModeNo)
}

// Resume implements backend.Resumer: it restores a previously suspended
// instance. The spec (plus this backend's configuration) must resolve to
// the state directory the suspend wrote to.
func (b *Backend) Resume(ctx context.Context, spec *vm.Spec) (backend.Instance, error) {
	return b.start(ctx, spec, vmm.ResumeModeForce)
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

func (b *Backend) start(ctx context.Context, spec *vm.Spec, resume vmm.ResumeMode) (backend.Instance, error) {
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
	return newInstance(handle, b.hasRemoteControl()), nil
}

// Instance is a virtual machine started by a Backend. Beyond the
// backend.Instance contract it implements the capabilities asserted
// above: suspend (paired with Backend.Resume), memory resizing over
// virtio-balloon, and device hotplug.
type Instance struct {
	vm               *vmm.VM
	hasRemoteControl bool
}

func newInstance(handle *vmm.VM, hasRemoteControl bool) *Instance {
	return &Instance{vm: handle, hasRemoteControl: hasRemoteControl}
}

// Wait blocks until the VM exits (or ctx is done), then releases runtime
// state.
func (i *Instance) Wait(ctx context.Context) error { return i.vm.Wait(ctx) }

// Kill hard-stops the VM immediately and releases runtime state, skipping
// the graceful guest shutdown path.
func (i *Instance) Kill() error { return i.vm.Kill() }

// Shutdown gracefully tears the instance down: guest shutdown (when the
// backend was configured with remote control), then QMP quit, then
// signals, then runtime cleanup; each step escalates on its own grace
// period. If ctx expires before teardown completes, the VM is killed
// instead, and the kill's result is returned. Shutdown is safe to call
// more than once and after the VM has exited.
func (i *Instance) Shutdown(ctx context.Context) error {
	closed := make(chan error, 1)
	go func() { closed <- i.vm.Close() }()
	select {
	case err := <-closed:
		return err
	case <-ctx.Done():
		// Kill waits for the in-flight Close to finish (teardown is
		// serialized), so the goroutine cannot outlive this call.
		return i.vm.Kill()
	}
}

// RemoteControl returns the guest-agent-backed vm.Guest, or an error
// wrapping errors.ErrUnsupported when the backend was configured without
// RemoteControl.
func (i *Instance) RemoteControl() (vm.Guest, error) {
	if !i.hasRemoteControl {
		return nil, fmt.Errorf("vm has no guest control agent: %w", errors.ErrUnsupported)
	}
	return &qgaGuest{vm: i.vm}, nil
}

// Suspend implements backend.Suspender: it saves the running instance's
// state via QMP migration into the state directory fixed at Start (the
// spec's Dir), then stops the VM. Restore it with Backend.Resume and the
// same spec.
func (i *Instance) Suspend(ctx context.Context) error { return i.vm.Suspend(ctx) }

// ResizeMemory implements backend.MemoryResizer via virtio-balloon. The
// backend must be configured with Backend.Balloon (or a manifest balloon
// device).
func (i *Instance) ResizeMemory(ctx context.Context, size units.Bytes) error {
	return i.vm.ResizeMemory(ctx, size.Int64())
}

// Attach implements backend.DeviceAttacher over QMP hotplug. The instance
// must have PCIe hotplug ports reserved at Start: set Backend.HotplugPorts
// (or a manifest [hotplug] section / hotplug.ports for manifest.Load
// backends).
func (i *Instance) Attach(ctx context.Context, dev vm.Device) error {
	hdev, err := hotplugDevice(i.vm, dev)
	if err != nil {
		return err
	}
	return i.vm.AttachHotplugDevice(ctx, hdev)
}

// Detach implements backend.DeviceAttacher; see Attach.
func (i *Instance) Detach(ctx context.Context, dev vm.Device) error {
	hdev, err := hotplugDevice(i.vm, dev)
	if err != nil {
		return err
	}
	return i.vm.DetachHotplugDevice(ctx, hdev.ID)
}

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
		forward := specForward(d)
		identity := forward.Proto + "\x00" + d.HostAddr + "\x00" + d.GuestAddr
		return resolver.ResolveHotplugNetwork(imanifest.NetworkInput{
			ID:      deviceID("fwd", identity),
			MAC:     deviceMAC(identity),
			Forward: []imanifest.ForwardPort{forward},
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
