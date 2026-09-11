package manifest

import (
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"time"

	"github.com/shazow/virtle/units"
)

// Defaults and bounds the API-driven microVM backends share.
const (
	defaultVMMTimeout = 10 * time.Second
	// maxVMMMemoryMiB bounds the guest memory the microVM sections accept:
	// far beyond what such a host backs, and small enough that the byte
	// conversion the backends send stays exact.
	maxVMMMemoryMiB = units.MiB(1 << 20)
)

// defaultVMMCPUs is the vCPU count for an omitted machine.vcpu: every host
// CPU, as QEMU derives it, within the VMM's limit.
func defaultVMMCPUs(limit int) int {
	return max(1, min(runtime.NumCPU(), limit))
}

// unsupportedBy reports a manifest setting that configures something the
// named backend cannot honor, wrapping errors.ErrUnsupported so callers can
// tell a missing capability from an invalid manifest.
func unsupportedBy(backend string, format string, args ...any) error {
	return fmt.Errorf(backend+": "+format+": %w", append(args, errors.ErrUnsupported)...)
}

// VMM is the launch configuration the API-driven microVM backends share:
// the executable and lifecycle timeouts, the machine size, the boot source,
// raw disks, TAP NICs, and the console mode, with paths already resolved
// against the working directory. Firecracker and CloudHypervisor embed it.
type VMM struct {
	Binary          string        `json:"binary"`
	StartupTimeout  time.Duration `json:"startupTimeout"`
	ShutdownTimeout time.Duration `json:"shutdownTimeout"`
	CPUs            int           `json:"cpus"`
	MemoryMiB       units.MiB     `json:"memoryMiB"`
	Kernel          BootSource    `json:"kernel"`
	Disks           []RawDisk     `json:"disks,omitempty"`
	// Networks are the guest NICs, each a host TAP device.
	Networks []TapNetwork `json:"networks,omitempty"`
	// Console is KernelSerialOff or KernelSerialPrint.
	Console string `json:"console"`
}

// TapNetwork is a guest NIC backed by a host TAP device: the host kernel
// provides the network, the VMM only moves frames.
type TapNetwork struct {
	ID  string `json:"id"`
	Tap string `json:"tap"`
	MAC string `json:"mac"`
}

// BootSource is the resolved kernel, initrd, and command line.
type BootSource struct {
	Path       string `json:"path"`
	InitrdPath string `json:"initrdPath,omitempty"`
	// Cmdline is the complete guest command line virtle passes to the VMM:
	// console parameters, the reboot/panic policy virtle has for the VMM (if
	// any), root= for the disk mounted at "/", then the manifest's
	// kernel.params. No drive is marked as the VMM's root device, so it
	// appends nothing itself.
	Cmdline string `json:"cmdline"`
}

// RawDisk is a resolved raw block device. Root marks the image mounted at
// "/"; virtle passes root= for it itself, so the VMM's own root-device boot
// arguments stay off. Create asks the backend to format a missing image as
// an empty ext4 filesystem of SizeMiB before launch, as QEMU does for
// image.create.
type RawDisk struct {
	Path     string    `json:"path"`
	ReadOnly bool      `json:"readOnly,omitempty"`
	Root     bool      `json:"root,omitempty"`
	Create   bool      `json:"create,omitempty"`
	SizeMiB  units.MiB `json:"sizeMiB,omitempty"`
	Label    string    `json:"label,omitempty"`
}

// vmmInput is the shape FirecrackerInput and CloudHypervisorInput share;
// either converts to it.
type vmmInput struct {
	Binary          string
	StartupTimeout  units.Duration
	ShutdownTimeout units.Duration
}

// vmmProfile is what distinguishes one microVM backend's resolution from the
// other's: its name, executable, vCPU bound, and kernel command line policy.
type vmmProfile struct {
	backend       string
	defaultBinary string
	maxCPUs       int
	cmdline       func(serialMode string, root, extra []string) string
}

// seededDocument is the document DecodeDocumentBytes decodes into: every
// scalar default tag applied and nothing else set. A section equal to its
// seeded (or zero) value was not configured by the manifest author.
func seededDocument() Document {
	var doc Document
	applyDefaultTags(&doc)
	return doc
}

// unconfigured reports whether section is the zero value or one of the
// baselines virtle produces itself (the decoder's seeded defaults, the
// DocumentWithDefaults output), i.e. the manifest author did not configure it.
func unconfigured[T any](section T, baselines ...T) bool {
	var zero T
	if reflect.DeepEqual(section, zero) {
		return true
	}
	for _, baseline := range baselines {
		if reflect.DeepEqual(section, baseline) {
			return true
		}
	}
	return false
}

// rejectQEMUOnly fails on sections that configure QEMU devices, host
// helpers, or guest-agent features, which neither microVM backend can honor,
// rather than dropping them, so a QEMU manifest switched over fails loudly
// instead of losing behavior. Values equal to what virtle itself defaults
// QEMU-only sections to (the user network, the ssh command) never count as
// configuration, so a decoded document that already went through
// DocumentWithDefaults resolves like the raw one.
func (d Document) rejectQEMUOnly(backend string) error {
	seeded, defaults := seededDocument(), DefaultDocument()
	switch {
	case !unconfigured(d.SSH, seeded.SSH, defaults.SSH):
		return unsupportedBy(backend, "manifest.ssh needs a guest control transport, which %s does not have yet", backend)
	case !unconfigured(d.VSock.CIDRange, defaults.VSock.CIDRange) || (d.VSock.Enabled != nil && *d.VSock.Enabled):
		return unsupportedBy(backend, "manifest.vsock: %s guests have no vsock device", backend)
	case !unconfigured(d.QEMU, seeded.QEMU, defaults.QEMU):
		return unsupportedBy(backend, "manifest.qemu configures QEMU; remove it or set backend = %q", BackendQEMU)
	case len(d.WriteFiles) != 0 || d.Workspace != (WorkspaceInput{}):
		return unsupportedBy(backend, "manifest.write_files and manifest.workspace need a guest control transport, which %s does not have yet", backend)
	case len(d.Run) != 0 || len(d.Notifications.Exec) != 0 || len(d.Notifications.States) != 0:
		return unsupportedBy(backend, "manifest.run and manifest.notifications")
	case d.Balloon != nil || d.Hotplug.Len() != 0:
		return unsupportedBy(backend, "manifest.balloon and manifest.hotplug")
	case d.Graphics != nil && d.Graphics.Backend != "" && d.Graphics.Backend != defaultGraphicsBackend:
		return unsupportedBy(backend, "manifest.graphics")
	case d.Machine.CPU != "" || d.Machine.ID != "" || (d.Machine.Type != "" && d.Machine.Type != seeded.Machine.Type):
		return unsupportedBy(backend, "manifest.machine.type, cpu and id configure QEMU")
	case d.Machine.KVM != nil && !*d.Machine.KVM:
		return unsupportedBy(backend, "%s requires KVM; manifest.machine.kvm cannot be false", backend)
	case d.Egress != nil:
		return unsupportedBy(backend, "manifest.egress needs a network of type virtle, which %s reaches only through the guest daemon", backend)
	}
	return nil
}

// tapNetworks lowers [[networks]] for a VMM whose only NIC is a host TAP
// device. The user entry virtle seeds by default means no NIC.
func tapNetworks(backend string, networks, defaults []NetworkInput) ([]TapNetwork, error) {
	if len(networks) == 0 || reflect.DeepEqual(networks, defaults) {
		return nil, nil
	}
	result := make([]TapNetwork, 0, len(networks))
	ids := make(map[string]bool, len(networks))
	for i, network := range networks {
		switch network.Type {
		case NetworkTypeTAP:
		case NetworkTypeVirtle:
			return nil, unsupportedBy(backend, "manifest.networks[%d].type virtle: %s reaches a virtle network through the guest daemon, which does not exist yet", i, backend)
		case "", NetworkTypeUser:
			return nil, unsupportedBy(backend, "manifest.networks[%d].type user: %s has no user networking; use type tap", i, backend)
		default:
			return nil, fmt.Errorf("manifest.networks[%d].type must be tap for %s", i, backend)
		}
		if err := validateTapName(network.Tap); err != nil {
			return nil, fmt.Errorf("manifest.networks[%d].tap %w", i, err)
		}
		if len(network.Forward) != 0 {
			return nil, unsupportedBy(backend, "manifest.networks[%d].forward on a tap network; the host kernel routes it", i)
		}
		id := network.ID
		if id == "" {
			id = defaultNetworkID
		}
		if ids[id] {
			return nil, fmt.Errorf("manifest.networks[%d].id %q is used twice", i, id)
		}
		ids[id] = true
		mac := network.MAC
		if mac == "" {
			mac = defaultNetworkMAC
		}
		if _, err := net.ParseMAC(mac); err != nil {
			return nil, fmt.Errorf("manifest.networks[%d].mac: %w", i, err)
		}
		result = append(result, TapNetwork{ID: id, Tap: network.Tap, MAC: mac})
	}
	return result, nil
}

// resolveVMM resolves what the microVM backends share, once the caller has
// rejected the sections its VMM cannot honor: the Manifest skeleton
// (identity, paths, lock, state directory) and the machine size, boot
// source, raw disks, TAP NICs, console mode, executable, and timeouts.
func (d Document) resolveVMM(p vmmProfile, in vmmInput) (*Manifest, *VMM, error) {
	networks, err := tapNetworks(p.backend, d.Networks, DefaultDocument().Networks)
	if err != nil {
		return nil, nil, err
	}
	d = DocumentWithDefaults(d)
	if err := validateHostName(d.HostName); err != nil {
		return nil, nil, err
	}
	if d.Kernel.Path == "" {
		return nil, nil, fmt.Errorf("manifest.kernel.path is required")
	}
	serialMode, err := kernelSerialMode(d.Kernel)
	if err != nil {
		return nil, nil, err
	}
	if serialMode == KernelSerialConsole {
		return nil, nil, unsupportedBy(p.backend, "manifest.kernel.serial = %q; use %q or %q", KernelSerialConsole, KernelSerialOff, KernelSerialPrint)
	}
	if d.Machine.VCPU < 0 || d.Machine.VCPU > p.maxCPUs {
		return nil, nil, fmt.Errorf("manifest.machine.vcpu must be between 1 and %d for %s, got %d", p.maxCPUs, p.backend, d.Machine.VCPU)
	}
	if d.Machine.Memory <= 0 || d.Machine.Memory > maxVMMMemoryMiB {
		return nil, nil, fmt.Errorf("manifest.machine.memory must be between 1 and %d MiB for %s, got %d", maxVMMMemoryMiB, p.backend, d.Machine.Memory)
	}
	rootIndex, rootParams, err := rootDevice(d.Mounts.Image())
	if err != nil {
		return nil, nil, err
	}
	if err := validateBootSource(d.Kernel, d.Mounts.Image(), rootIndex); err != nil {
		return nil, nil, err
	}
	if err := d.ResolveWorkingDir(); err != nil {
		return nil, nil, err
	}
	m := &Manifest{
		Backend:     p.backend,
		Identity:    Identity{HostName: d.HostName},
		Paths:       Paths{WorkingDir: d.WorkingDir},
		Persistence: Persistence{BaseDir: d.StateDir, StateDir: d.StateDir},
	}
	m.Paths.LockPath = filepath.Join(m.Persistence.StateDir, m.Identity.HostName+".lock")
	m.Paths.RuntimeDir = RuntimeDir{Mode: RuntimeDirPath, Path: m.Persistence.StateDir}

	vmm := &VMM{
		Binary:          in.Binary,
		StartupTimeout:  in.StartupTimeout.Duration(),
		ShutdownTimeout: in.ShutdownTimeout.Duration(),
		CPUs:            d.Machine.VCPU,
		MemoryMiB:       d.Machine.Memory,
		Kernel: BootSource{
			Path:       m.resolvePath(d.Kernel.Path),
			InitrdPath: m.resolvePath(d.Kernel.InitrdPath),
			Cmdline:    p.cmdline(serialMode, rootParams, d.Kernel.Params),
		},
		Networks: networks,
		Console:  serialMode,
	}
	switch {
	case vmm.Binary == "":
		vmm.Binary = p.defaultBinary
	case strings.ContainsRune(vmm.Binary, '/'):
		// A bare name is looked up on PATH; a path resolves like image paths.
		vmm.Binary = m.resolvePath(vmm.Binary)
	}
	if vmm.CPUs == 0 {
		vmm.CPUs = defaultVMMCPUs(p.maxCPUs)
	}
	if vmm.StartupTimeout < 0 || vmm.ShutdownTimeout < 0 {
		return nil, nil, fmt.Errorf("manifest.%s timeouts must not be negative", p.backend)
	}
	if vmm.StartupTimeout == 0 {
		vmm.StartupTimeout = defaultVMMTimeout
	}
	if vmm.ShutdownTimeout == 0 {
		vmm.ShutdownTimeout = defaultVMMTimeout
	}
	for i, mount := range d.Mounts.Image() {
		switch {
		case mount.SourcePath == "":
			return nil, nil, fmt.Errorf("manifest.mounts[%d].source is required", i)
		case mount.Image.Format != "" && mount.Image.Format != "raw":
			return nil, nil, unsupportedBy(p.backend, "manifest.mounts[%d].image.format %q; only raw images are supported", i, mount.Image.Format)
		case mount.Image.FSType != "" && mount.Image.FSType != defaultVolumeFSType:
			return nil, nil, unsupportedBy(p.backend, "manifest.mounts[%d].image.fs %q; created images are %s", i, mount.Image.FSType, defaultVolumeFSType)
		case mount.Image.AutoCreate && mount.Image.Size < minAutoVolumeSize:
			return nil, nil, fmt.Errorf("manifest.mounts[%d].image.size must be at least %d when image.create is true, got %d", i, minAutoVolumeSize, mount.Image.Size)
		case mount.Image.Serial != nil || mount.Image.Direct:
			return nil, nil, unsupportedBy(p.backend, "manifest.mounts[%d] image.serial and image.direct", i)
		}
		vmm.Disks = append(vmm.Disks, RawDisk{
			Path:     m.resolvePath(mount.SourcePath),
			ReadOnly: mount.ReadOnly,
			Root:     i == rootIndex,
			Create:   mount.Image.AutoCreate,
			SizeMiB:  mount.Image.Size,
			Label:    stringValue(mount.Image.Label),
		})
	}
	return m, vmm, nil
}
