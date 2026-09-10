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

// Firecracker defaults applied when the manifest leaves a value unset, and
// the VMM's own limits.
const (
	defaultFirecrackerBinary  = "firecracker"
	defaultFirecrackerTimeout = 10 * time.Second
	// MaxFirecrackerCPUs is the largest vCPU count Firecracker accepts.
	MaxFirecrackerCPUs      = 32
	maxFirecrackerMemoryMiB = units.MiB(1 << 20)
)

// defaultFirecrackerCPUs is the vCPU count for an omitted machine.vcpu: every
// host CPU, as QEMU derives it, within Firecracker's limit.
func defaultFirecrackerCPUs() int {
	return max(1, min(runtime.NumCPU(), MaxFirecrackerCPUs))
}

// unsupported reports a manifest setting that configures something the
// Firecracker backend cannot honor, wrapping errors.ErrUnsupported so callers
// can tell a missing capability from an invalid manifest.
func unsupported(format string, args ...any) error {
	return fmt.Errorf("firecracker: "+format+": %w", append(args, errors.ErrUnsupported)...)
}

// FirecrackerInput contains only VMM-specific options. Resources and boot
// devices use the same machine, kernel and mounts sections as QEMU.
type FirecrackerInput struct {
	Binary          string         `json:"binary,omitempty" toml:"binary" jsonschema:"Firecracker executable; default firecracker. No shell expansion."`
	StartupTimeout  units.Duration `json:"startup_timeout,omitempty" toml:"startup_timeout" jsonschema:"Maximum time for API startup and configuration; zero uses 10s."`
	ShutdownTimeout units.Duration `json:"shutdown_timeout,omitempty" toml:"shutdown_timeout" jsonschema:"Maximum graceful shutdown time; zero uses 10s."`
}

// Firecracker is the resolved Firecracker launch configuration: what the
// backend puts on the Firecracker API, with paths already resolved against
// the working directory.
type Firecracker struct {
	Binary          string            `json:"binary"`
	StartupTimeout  time.Duration     `json:"startupTimeout"`
	ShutdownTimeout time.Duration     `json:"shutdownTimeout"`
	CPUs            int               `json:"cpus"`
	MemoryMiB       units.MiB         `json:"memoryMiB"`
	Kernel          FirecrackerKernel `json:"kernel"`
	Disks           []FirecrackerDisk `json:"disks,omitempty"`
	// Networks are the guest NICs, each a host TAP device.
	Networks []FirecrackerNetwork `json:"networks,omitempty"`
	// Console is KernelSerialOff or KernelSerialPrint.
	Console string `json:"console"`
}

// FirecrackerNetwork is a guest NIC backed by a host TAP device: the host
// kernel provides the network, Firecracker only moves frames.
type FirecrackerNetwork struct {
	ID  string `json:"id"`
	Tap string `json:"tap"`
	MAC string `json:"mac"`
}

// FirecrackerKernel is the resolved boot source.
type FirecrackerKernel struct {
	Path       string `json:"path"`
	InitrdPath string `json:"initrdPath,omitempty"`
	// Cmdline is the complete guest command line virtle passes to the API:
	// console parameters, virtle's reboot/panic policy, root= for the disk
	// mounted at "/", then the manifest's kernel.params. No drive is marked
	// as Firecracker's root device, so Firecracker appends nothing itself.
	Cmdline string `json:"cmdline"`
}

// FirecrackerDisk is a resolved raw block device. Root marks the image
// mounted at "/"; virtle passes root= for it itself, so Firecracker's own
// root-device boot arguments stay off. Create asks the backend to format a
// missing image as an empty ext4 filesystem of SizeMiB before launch, as
// QEMU does for image.create.
type FirecrackerDisk struct {
	Path     string    `json:"path"`
	ReadOnly bool      `json:"readOnly,omitempty"`
	Root     bool      `json:"root,omitempty"`
	Create   bool      `json:"create,omitempty"`
	SizeMiB  units.MiB `json:"sizeMiB,omitempty"`
	Label    string    `json:"label,omitempty"`
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

// firecrackerNetworks lowers [[networks]] for Firecracker, whose only NIC is
// a host TAP device. The user entry virtle seeds by default means no NIC,
// as it did before Firecracker had any.
func firecrackerNetworks(networks, defaults []NetworkInput) ([]FirecrackerNetwork, error) {
	if len(networks) == 0 || reflect.DeepEqual(networks, defaults) {
		return nil, nil
	}
	result := make([]FirecrackerNetwork, 0, len(networks))
	ids := make(map[string]bool, len(networks))
	for i, network := range networks {
		switch network.Type {
		case NetworkTypeTAP:
		case NetworkTypeVirtle:
			return nil, unsupported("manifest.networks[%d].type virtle: firecracker reaches a virtle network through the guest daemon, which does not exist yet", i)
		case "", NetworkTypeUser:
			return nil, unsupported("manifest.networks[%d].type user: firecracker has no user networking; use type tap", i)
		default:
			return nil, fmt.Errorf("manifest.networks[%d].type must be tap for firecracker", i)
		}
		if err := validateTapName(network.Tap); err != nil {
			return nil, fmt.Errorf("manifest.networks[%d].tap %w", i, err)
		}
		if len(network.Forward) != 0 {
			return nil, unsupported("manifest.networks[%d].forward on a tap network; the host kernel routes it", i)
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
		result = append(result, FirecrackerNetwork{ID: id, Tap: network.Tap, MAC: mac})
	}
	return result, nil
}

// firecrackerManifest resolves a backend = "firecracker" document. Sections
// that configure QEMU devices, host helpers, or guest-agent features are
// rejected rather than silently dropped, so a QEMU manifest switched to
// Firecracker fails loudly instead of losing behavior. Values equal to what
// virtle itself defaults QEMU-only sections to (the user network, the ssh
// command) never count as configuration, so a decoded document that already
// went through DocumentWithDefaults resolves like the raw one.
func (d Document) firecrackerManifest() (*Manifest, error) {
	seeded, defaults := seededDocument(), DefaultDocument()
	switch {
	case !unconfigured(d.SSH, seeded.SSH, defaults.SSH):
		return nil, unsupported("manifest.ssh needs a guest control transport, which firecracker does not have yet")
	case !unconfigured(d.VSock.CIDRange, defaults.VSock.CIDRange) || (d.VSock.Enabled != nil && *d.VSock.Enabled):
		return nil, unsupported("manifest.vsock: firecracker guests have no vsock device")
	case !unconfigured(d.QEMU, seeded.QEMU, defaults.QEMU):
		return nil, unsupported("manifest.qemu configures QEMU; remove it or set backend = %q", BackendQEMU)
	case len(d.WriteFiles) != 0 || d.Workspace != (WorkspaceInput{}):
		return nil, unsupported("manifest.write_files and manifest.workspace need a guest control transport, which firecracker does not have yet")
	case len(d.Run) != 0 || len(d.Notifications.Exec) != 0 || len(d.Notifications.States) != 0:
		return nil, unsupported("manifest.run and manifest.notifications")
	case d.Balloon != nil || d.Hotplug.Len() != 0:
		return nil, unsupported("manifest.balloon and manifest.hotplug")
	case d.Graphics != nil && d.Graphics.Backend != "" && d.Graphics.Backend != defaultGraphicsBackend:
		return nil, unsupported("manifest.graphics")
	case d.Machine.CPU != "" || d.Machine.ID != "" || (d.Machine.Type != "" && d.Machine.Type != seeded.Machine.Type):
		return nil, unsupported("manifest.machine.type, cpu and id configure QEMU")
	case d.Machine.KVM != nil && !*d.Machine.KVM:
		return nil, unsupported("firecracker requires KVM; manifest.machine.kvm cannot be false")
	case len(d.Mounts) != len(d.Mounts.Image()):
		return nil, unsupported("only image mounts are supported")
	case d.Egress != nil:
		return nil, unsupported("manifest.egress needs a network of type virtle, which firecracker reaches only through the guest daemon")
	}
	networks, err := firecrackerNetworks(d.Networks, defaults.Networks)
	if err != nil {
		return nil, err
	}

	d = DocumentWithDefaults(d)
	if err := validateHostName(d.HostName); err != nil {
		return nil, err
	}
	if d.Kernel.Path == "" {
		return nil, fmt.Errorf("manifest.kernel.path is required")
	}
	serialMode, err := kernelSerialMode(d.Kernel)
	if err != nil {
		return nil, err
	}
	if serialMode == KernelSerialConsole {
		return nil, unsupported("manifest.kernel.serial = %q; use %q or %q", KernelSerialConsole, KernelSerialOff, KernelSerialPrint)
	}
	if d.Machine.VCPU < 0 || d.Machine.VCPU > MaxFirecrackerCPUs {
		return nil, fmt.Errorf("manifest.machine.vcpu must be between 1 and %d for firecracker, got %d", MaxFirecrackerCPUs, d.Machine.VCPU)
	}
	if d.Machine.Memory <= 0 || d.Machine.Memory > maxFirecrackerMemoryMiB {
		return nil, fmt.Errorf("manifest.machine.memory must be between 1 and %d MiB for firecracker, got %d", maxFirecrackerMemoryMiB, d.Machine.Memory)
	}
	rootIndex, rootParams, err := rootDevice(d.Mounts.Image())
	if err != nil {
		return nil, err
	}
	if err := validateBootSource(d.Kernel, d.Mounts.Image(), rootIndex); err != nil {
		return nil, err
	}
	if err := d.ResolveWorkingDir(); err != nil {
		return nil, err
	}
	m := &Manifest{
		Backend:     BackendFirecracker,
		Identity:    Identity{HostName: d.HostName},
		Paths:       Paths{WorkingDir: d.WorkingDir},
		Persistence: Persistence{BaseDir: d.StateDir, StateDir: d.StateDir},
	}
	m.Paths.LockPath = filepath.Join(m.Persistence.StateDir, m.Identity.HostName+".lock")
	m.Paths.RuntimeDir = RuntimeDir{Mode: RuntimeDirPath, Path: m.Persistence.StateDir}

	fc := &Firecracker{
		Binary:          d.Firecracker.Binary,
		StartupTimeout:  d.Firecracker.StartupTimeout.Duration(),
		ShutdownTimeout: d.Firecracker.ShutdownTimeout.Duration(),
		CPUs:            d.Machine.VCPU,
		MemoryMiB:       d.Machine.Memory,
		Kernel: FirecrackerKernel{
			Path:       m.resolvePath(d.Kernel.Path),
			InitrdPath: m.resolvePath(d.Kernel.InitrdPath),
			Cmdline:    firecrackerKernelParams(serialMode, rootParams, d.Kernel.Params),
		},
		Networks: networks,
		Console:  serialMode,
	}
	switch {
	case fc.Binary == "":
		fc.Binary = defaultFirecrackerBinary
	case strings.ContainsRune(fc.Binary, '/'):
		// A bare name is looked up on PATH; a path resolves like image paths.
		fc.Binary = m.resolvePath(fc.Binary)
	}
	if fc.CPUs == 0 {
		fc.CPUs = defaultFirecrackerCPUs()
	}
	if fc.StartupTimeout < 0 || fc.ShutdownTimeout < 0 {
		return nil, fmt.Errorf("manifest.firecracker timeouts must not be negative")
	}
	if fc.StartupTimeout == 0 {
		fc.StartupTimeout = defaultFirecrackerTimeout
	}
	if fc.ShutdownTimeout == 0 {
		fc.ShutdownTimeout = defaultFirecrackerTimeout
	}
	for i, mount := range d.Mounts.Image() {
		switch {
		case mount.SourcePath == "":
			return nil, fmt.Errorf("manifest.mounts[%d].source is required", i)
		case mount.Image.Format != "" && mount.Image.Format != "raw":
			return nil, unsupported("manifest.mounts[%d].image.format %q; only raw images are supported", i, mount.Image.Format)
		case mount.Image.FSType != "" && mount.Image.FSType != defaultVolumeFSType:
			return nil, unsupported("manifest.mounts[%d].image.fs %q; created images are %s", i, mount.Image.FSType, defaultVolumeFSType)
		case mount.Image.AutoCreate && mount.Image.Size < minAutoVolumeSize:
			return nil, fmt.Errorf("manifest.mounts[%d].image.size must be at least %d when image.create is true, got %d", i, minAutoVolumeSize, mount.Image.Size)
		case mount.Image.Serial != nil || mount.Image.Direct:
			return nil, unsupported("manifest.mounts[%d] image.serial and image.direct", i)
		}
		fc.Disks = append(fc.Disks, FirecrackerDisk{
			Path:     m.resolvePath(mount.SourcePath),
			ReadOnly: mount.ReadOnly,
			Root:     i == rootIndex,
			Create:   mount.Image.AutoCreate,
			SizeMiB:  mount.Image.Size,
			Label:    stringValue(mount.Image.Label),
		})
	}
	m.Firecracker = fc
	return m, nil
}

// firecrackerKernelParams assembles the guest command line the same way
// kernelParams does for QEMU: console parameters for the serial mode, then
// virtle's fixed reboot/panic policy, the root device, then the manifest's
// own parameters. Firecracker exits on the i8042 reset that reboot=k
// requests, which is also how Shutdown's Ctrl-Alt-Del completes; panic=-1
// turns a guest panic into that reset. Both VMMs expose the serial console
// as ttyS0.
func firecrackerKernelParams(serialMode string, root []string, extra []string) string {
	params := make([]string, 0, len(extra)+len(root)+3)
	if serialMode != KernelSerialOff {
		params = append(params, "console=ttyS0")
	}
	params = append(params, "reboot=k", "panic=-1")
	params = append(params, root...)
	params = append(params, extra...)
	return strings.Join(params, " ")
}
