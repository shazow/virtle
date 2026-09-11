package manifest

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/shazow/virtle/units"
)

// Cloud Hypervisor defaults applied when the manifest leaves a value unset,
// and the VMM's own limits.
const (
	defaultCloudHypervisorBinary = "cloud-hypervisor"
)

// MaxCloudHypervisorCPUs is the largest vCPU count Cloud Hypervisor accepts
// on this architecture; the VMM checks the host's own KVM limit on top when
// the VM is created.
var MaxCloudHypervisorCPUs = maxCloudHypervisorCPUs(runtime.GOARCH)

// maxCloudHypervisorCPUs is Cloud Hypervisor's MAX_SUPPORTED_CPUS: 8192 on
// x86_64, 255 elsewhere.
func maxCloudHypervisorCPUs(arch string) int {
	if arch == "amd64" {
		return 8192
	}
	return 255
}

// CloudHypervisorInput contains only VMM-specific options. Resources, boot
// devices, and virtiofs shares use the same machine, kernel and mounts
// sections as QEMU.
type CloudHypervisorInput struct {
	Binary          string         `json:"binary,omitempty" toml:"binary" jsonschema:"Cloud Hypervisor executable; default cloud-hypervisor. No shell expansion."`
	StartupTimeout  units.Duration `json:"startup_timeout,omitempty" toml:"startup_timeout" jsonschema:"Maximum time for API startup, share daemons, and configuration; zero uses 10s."`
	ShutdownTimeout units.Duration `json:"shutdown_timeout,omitempty" toml:"shutdown_timeout" jsonschema:"Maximum graceful shutdown time; zero uses 10s."`
}

// CloudHypervisor is the resolved Cloud Hypervisor launch configuration:
// what the backend puts in the VmConfig it creates over the API. Shares
// make the guest memory shared (virtio-fs needs the vhost-user daemon to
// map it); their virtiofsd processes are the manifest's Run entries.
type CloudHypervisor struct {
	VMM
	Shares []CloudHypervisorShare `json:"shares,omitempty"`
}

// CloudHypervisorShare is a resolved virtio-fs share: the tag the guest
// mounts, the host directory, and the vhost-user socket virtiofsd serves it
// on. The daemon is one of the manifest's Run entries unless the mount named
// a socket that is already live, which is then taken to be externally
// managed, as on QEMU.
type CloudHypervisorShare struct {
	Tag    string `json:"tag"`
	Source string `json:"source"`
	Socket string `json:"socket"`
}

// cloudHypervisorManifest resolves a backend = "cloud-hypervisor" document:
// the shared microVM resolution plus virtiofs shares.
func (d Document) cloudHypervisorManifest(options ResolveOptions) (*Manifest, error) {
	if err := d.rejectQEMUOnly(BackendCloudHypervisor); err != nil {
		return nil, err
	}
	if d.Firecracker != (FirecrackerInput{}) {
		return nil, unsupportedBy(BackendCloudHypervisor, "manifest.firecracker configures Firecracker; remove it or set backend = %q", BackendFirecracker)
	}
	if i, kind, ok := d.Mounts.firstMountNot(MountTypeImage, MountTypeVirtioFS); ok {
		return nil, unsupportedBy(BackendCloudHypervisor, "manifest.mounts[%d].type %s; shares are virtiofs mounts", i, kind)
	}
	m, vmm, err := d.resolveVMM(vmmProfile{
		backend:       BackendCloudHypervisor,
		defaultBinary: defaultCloudHypervisorBinary,
		maxCPUs:       MaxCloudHypervisorCPUs,
		cmdline: func(serialMode string, root, extra []string) string {
			return cloudHypervisorKernelParams(runtime.GOARCH, serialMode, root, extra)
		},
	}, vmmInput(d.CloudHypervisor))
	if err != nil {
		return nil, err
	}
	shares, err := m.resolveCloudHypervisorShares(d.Mounts.VirtioFS(), options)
	if err != nil {
		return nil, err
	}
	m.CloudHypervisor = &CloudHypervisor{VMM: *vmm, Shares: shares}
	return m, nil
}

// resolveCloudHypervisorShares lowers the virtiofs mounts. A mount without
// a socket gets <tag>.sock under the state directory and virtle's own
// virtiofsd, as on QEMU; the daemons become Run entries and their sockets
// cleanup files through the same resolution QEMU uses, so a socket that is
// already live is left to whoever serves it.
func (m *Manifest) resolveCloudHypervisorShares(mounts []VirtioFSMountInput, options ResolveOptions) ([]CloudHypervisorShare, error) {
	if len(mounts) == 0 {
		return nil, nil
	}
	tags := make(map[string]bool, len(mounts))
	for i := range mounts {
		mount := &mounts[i]
		switch {
		case mount.Tag == "":
			return nil, fmt.Errorf("manifest.mounts.virtiofs[%d].tag is required", i)
		case tags[mount.Tag]:
			return nil, fmt.Errorf("manifest.mounts.virtiofs[%d].tag %q is used twice", i, mount.Tag)
		case mount.SourcePath == "":
			return nil, fmt.Errorf("manifest.mounts.virtiofs[%d].source is required", i)
		}
		tags[mount.Tag] = true
		defaultVirtioFSDaemon(mount)
	}
	runs, err := m.resolveVirtioFSRuns(mounts, options)
	if err != nil {
		return nil, err
	}
	m.Run = append(m.Run, runs...)
	shares := make([]CloudHypervisorShare, 0, len(mounts))
	for _, mount := range mounts {
		socket, err := m.resolveSocketPath(mount.VirtioFS.Socket)
		if err != nil {
			return nil, err
		}
		shares = append(shares, CloudHypervisorShare{
			Tag:    mount.Tag,
			Source: m.resolvePath(mount.SourcePath),
			Socket: socket,
		})
	}
	return shares, nil
}

// cloudHypervisorConsole names the guest's serial device: Cloud Hypervisor
// emulates a 16550 on x86_64 and a PL011 on aarch64.
func cloudHypervisorConsole(arch string) string {
	if arch == "arm64" {
		return "ttyAMA0"
	}
	return "ttyS0"
}

// cloudHypervisorKernelParams assembles the guest command line: the serial
// console for the serial mode, the root device, then the manifest's own
// parameters. Unlike the other backends it carries no reboot=/panic=
// policy: Cloud Hypervisor restarts the VM in place on a guest reset, so
// panic=-1 would turn a kernel panic into a boot loop. A panicking guest
// halts instead until the host stops it, and the guest's shutdown path must
// be a power-off, which does end the VMM.
func cloudHypervisorKernelParams(arch, serialMode string, root []string, extra []string) string {
	params := make([]string, 0, len(extra)+len(root)+1)
	if serialMode != KernelSerialOff {
		params = append(params, "console="+cloudHypervisorConsole(arch))
	}
	params = append(params, root...)
	params = append(params, extra...)
	return strings.Join(params, " ")
}
