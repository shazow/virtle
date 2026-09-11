package manifest

import (
	"strings"

	"github.com/shazow/virtle/units"
)

// Firecracker defaults applied when the manifest leaves a value unset, and
// the VMM's own limits.
const (
	defaultFirecrackerBinary = "firecracker"
	// MaxFirecrackerCPUs is the largest vCPU count Firecracker accepts.
	MaxFirecrackerCPUs = 32
)

// FirecrackerInput contains only VMM-specific options. Resources and boot
// devices use the same machine, kernel and mounts sections as QEMU.
type FirecrackerInput struct {
	Binary          string         `json:"binary,omitempty" toml:"binary" jsonschema:"Firecracker executable; default firecracker. No shell expansion."`
	StartupTimeout  units.Duration `json:"startup_timeout,omitempty" toml:"startup_timeout" jsonschema:"Maximum time for API startup and configuration; zero uses 10s."`
	ShutdownTimeout units.Duration `json:"shutdown_timeout,omitempty" toml:"shutdown_timeout" jsonschema:"Maximum graceful shutdown time; zero uses 10s."`
}

// Firecracker is the resolved Firecracker launch configuration: what the
// backend puts on the Firecracker API.
type Firecracker struct {
	VMM
}

// firecrackerManifest resolves a backend = "firecracker" document: the
// shared microVM resolution, with image mounts the only mount kind.
func (d Document) firecrackerManifest() (*Manifest, error) {
	if err := d.rejectQEMUOnly(BackendFirecracker); err != nil {
		return nil, err
	}
	if d.CloudHypervisor != (CloudHypervisorInput{}) {
		return nil, unsupportedBy(BackendFirecracker, "manifest.cloud-hypervisor configures Cloud Hypervisor; remove it or set backend = %q", BackendCloudHypervisor)
	}
	if i, kind, ok := d.Mounts.firstMountNot(MountTypeImage); ok {
		return nil, unsupportedBy(BackendFirecracker, "manifest.mounts[%d].type %s; only image mounts are supported", i, kind)
	}
	m, vmm, err := d.resolveVMM(vmmProfile{
		backend:       BackendFirecracker,
		defaultBinary: defaultFirecrackerBinary,
		maxCPUs:       MaxFirecrackerCPUs,
		cmdline:       firecrackerKernelParams,
	}, vmmInput(d.Firecracker))
	if err != nil {
		return nil, err
	}
	m.Firecracker = &Firecracker{VMM: *vmm}
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
