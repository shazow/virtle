// Package qemu implements a virtle backend that launches virtual machines
// with QEMU. Constructors select the guest-control implementation wired
// into Instance.RemoteControl: BackendWithQGA (the QEMU Guest Agent,
// equivalent to the virtle CLI today) now, a virtle-native guest daemon
// variant later.
package qemu

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/internal/hotplug"
	"github.com/shazow/virtle/internal/manager"
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

// Config holds the QEMU-only knobs. The zero value works.
type Config struct {
	Binary         string            // default: qemu-system-<arch>
	Machine        string            // QEMU machine type; default: microvm
	MachineOptions map[string]string // extra machine options
	CPUModel       string            // QEMU CPU model; default derived per host
	KVM            *bool             // enable KVM acceleration; default derived per host
	ExtraArgs      []string          // passthrough QEMU arguments
	Console        Console
	Seccomp        bool         // enable QEMU seccomp sandboxing
	Balloon        bool         // attach a virtio-balloon device (required for ResizeMemory)
	HostName       string       // guest-visible name; default: "virtle"
	Logger         *slog.Logger // default: discard
}

// BackendWithQGA returns a QEMU backend whose instances' RemoteControl
// speaks the QEMU Guest Agent — the guest must run qemu-guest-agent on the
// default virtio-serial channel, as with the virtle CLI today.
func BackendWithQGA(cfg Config) (backend.Backend, error) {
	return &qemuBackend{cfg: cfg, hasRemoteControl: true}, nil
}

// BackendWithoutRemoteControl returns a QEMU backend for images that run
// no guest control agent: guest-dependent features (guest file writes,
// workspace mounts, graceful guest shutdown) are disabled and
// RemoteControl reports errors.ErrUnsupported.
func BackendWithoutRemoteControl(cfg Config) (backend.Backend, error) {
	return &qemuBackend{cfg: cfg}, nil
}

type qemuBackend struct {
	cfg              Config
	hasRemoteControl bool
	doc              *imanifest.Document // non-nil for manifest.Load-configured backends
}

// NewBackendFromDocument is the bridge for the public manifest package:
// the returned backend starts from the loaded document, preserving
// manifest sections that have no vm.Spec equivalent, and overlays the Spec
// passed to Start on top. The document type is internal, so this is not
// callable (and not supported) outside the module.
func NewBackendFromDocument(doc imanifest.Document, cfg Config) backend.Backend {
	// Manifests describe agent-equipped guests today, matching the CLI.
	return &qemuBackend{cfg: cfg, hasRemoteControl: true, doc: &doc}
}

func (b *qemuBackend) Start(ctx context.Context, spec *vm.Spec) (backend.Instance, error) {
	return b.start(ctx, spec, manager.ResumeModeNo)
}

func (b *qemuBackend) start(ctx context.Context, spec *vm.Spec, resume manager.ResumeMode) (backend.Instance, error) {
	doc, err := specDocument(spec, b.cfg, b.doc)
	if err != nil {
		return nil, err
	}
	logger := b.cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	mf, err := doc.ManifestWithOptions(imanifest.ResolveOptions{Logger: logger})
	if err != nil {
		return nil, fmt.Errorf("resolve vm spec: %w", err)
	}
	handle, err := manager.StartVM(ctx, mf, manager.StartOptions{
		Resume:           resume,
		HasRemoteControl: b.hasRemoteControl,
	}, manager.Config{
		Logger: logger,
		// A never-firing signal channel keeps the launch lifecycle from
		// installing process-global signal handlers: signal policy belongs
		// to the embedding program, not the library.
		Signals: make(chan os.Signal),
	})
	if err != nil {
		return nil, err
	}
	return &instance{vm: handle, hasRemoteControl: b.hasRemoteControl}, nil
}

type instance struct {
	vm               *manager.VM
	hasRemoteControl bool
}

func (i *instance) Wait(ctx context.Context) error { return i.vm.Wait(ctx) }

func (i *instance) Kill() error { return i.vm.Kill() }

func (i *instance) RemoteControl() (vm.Guest, error) {
	if !i.hasRemoteControl {
		return nil, fmt.Errorf("vm has no guest control agent: %w", errors.ErrUnsupported)
	}
	return &qgaGuest{vm: i.vm}, nil
}

// Close gracefully tears the instance down (guest shutdown, then QMP quit,
// then signals, then runtime cleanup). Wait and Kill already release
// runtime state; Close covers callers abandoning a VM without waiting.
func (i *instance) Close() error { return i.vm.Close() }

func (b *qemuBackend) ownInstance(inst backend.Instance) (*instance, error) {
	i, ok := inst.(*instance)
	if !ok {
		return nil, fmt.Errorf("instance was not started by this backend")
	}
	return i, nil
}

// Suspend implements backend.Suspender: it saves the running instance's
// state via QMP migration and stops the VM. The state lands in the
// instance's own state directory (derived from its Spec); a differing
// stateDir is rejected because socket paths and lock files are anchored
// there at Start. Pass "" to use the instance's directory.
func (b *qemuBackend) Suspend(ctx context.Context, inst backend.Instance, stateDir string) error {
	i, err := b.ownInstance(inst)
	if err != nil {
		return err
	}
	if stateDir != "" && stateDir != i.vm.StateDir() {
		return fmt.Errorf("suspend state dir is fixed at Start (%q): %w", i.vm.StateDir(), errors.ErrUnsupported)
	}
	return i.vm.Suspend(ctx)
}

// Resume implements backend.Suspender: it restores a previously suspended
// instance. The spec (plus this backend's config) must resolve to the same
// state directory the suspend wrote to.
func (b *qemuBackend) Resume(ctx context.Context, spec *vm.Spec, stateDir string) (backend.Instance, error) {
	inst, err := b.start(ctx, spec, manager.ResumeModeForce)
	if err != nil {
		return nil, err
	}
	i := inst.(*instance)
	if stateDir != "" && stateDir != i.vm.StateDir() {
		return nil, errors.Join(
			fmt.Errorf("resume state dir %q does not match the spec's state dir %q", stateDir, i.vm.StateDir()),
			i.Kill(),
		)
	}
	return inst, nil
}

// ResizeMemory implements backend.MemoryResizer via virtio-balloon. The
// backend must be configured with Config.Balloon (or a manifest balloon
// device).
func (b *qemuBackend) ResizeMemory(ctx context.Context, inst backend.Instance, size units.Bytes) error {
	i, err := b.ownInstance(inst)
	if err != nil {
		return err
	}
	return i.vm.ResizeMemory(ctx, size.Int64())
}

// Attach implements backend.DeviceAttacher over QMP hotplug. The instance
// must have PCIe hotplug ports reserved at Start, which today requires a
// manifest [hotplug] section (manifest.Load backends); ad hoc specs cannot
// reserve ports yet.
func (b *qemuBackend) Attach(ctx context.Context, inst backend.Instance, dev vm.Device) error {
	i, err := b.ownInstance(inst)
	if err != nil {
		return err
	}
	hdev, err := hotplugDevice(dev)
	if err != nil {
		return err
	}
	return i.vm.AttachHotplugDevice(ctx, hdev)
}

// Detach implements backend.DeviceAttacher; see Attach.
func (b *qemuBackend) Detach(ctx context.Context, inst backend.Instance, dev vm.Device) error {
	i, err := b.ownInstance(inst)
	if err != nil {
		return err
	}
	hdev, err := hotplugDevice(dev)
	if err != nil {
		return err
	}
	return i.vm.DetachHotplugDevice(ctx, hdev)
}

// hotplugDevice maps the sealed vm.Device union onto the internal hotplug
// device description.
func hotplugDevice(dev vm.Device) (hotplug.Device, error) {
	switch d := dev.(type) {
	case vm.Share:
		if d.Tag == "" {
			return hotplug.Device{}, fmt.Errorf("share device: Tag is required")
		}
		return hotplug.Device{
			Kind: hotplug.KindVirtioFS,
			ID:   d.Tag,
			VirtioFS: hotplug.VirtioFS{
				Source: d.HostPath,
				Target: d.GuestPath,
			},
		}, nil
	case vm.Disk:
		if d.Path == "" {
			return hotplug.Device{}, fmt.Errorf("disk device: Path is required")
		}
		return hotplug.Device{
			Kind: hotplug.KindBlock,
			ID:   deviceID("disk", d.Path),
			Block: hotplug.Block{
				ImagePath: d.Path,
				Format:    d.Format,
			},
		}, nil
	case vm.Forward:
		proto := d.Proto
		if proto == "" {
			proto = "tcp"
		}
		return hotplug.Device{
			Kind: hotplug.KindNet,
			ID:   deviceID("fwd", proto+"-"+d.HostAddr),
			Net: hotplug.Net{
				Backend: "user",
				Forward: []hotplug.Forward{{Proto: proto, Host: d.HostAddr, Guest: d.GuestAddr}},
			},
		}, nil
	default:
		return hotplug.Device{}, fmt.Errorf("unsupported device type %T", dev)
	}
}

// deviceID derives a stable hotplug ID from a device's identity, keeping
// it filesystem- and QEMU-id-safe.
func deviceID(kind, identity string) string {
	safe := make([]rune, 0, len(identity))
	for _, r := range identity {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.':
			safe = append(safe, r)
		default:
			safe = append(safe, '_')
		}
	}
	return kind + "-" + string(safe)
}
