package manifest

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/shazow/virtle/units"
)

// FirecrackerInput contains only VMM-specific options. Resources and boot
// devices use the same machine, kernel and mounts sections as QEMU.
type FirecrackerInput struct {
	Binary          string         `json:"binary,omitempty" toml:"binary" jsonschema:"Firecracker executable; default firecracker. No shell expansion."`
	StartupTimeout  units.Duration `json:"startup_timeout,omitempty" toml:"startup_timeout" jsonschema:"Maximum time for API startup and configuration; zero uses 10s."`
	ShutdownTimeout units.Duration `json:"shutdown_timeout,omitempty" toml:"shutdown_timeout" jsonschema:"Maximum graceful shutdown time; zero uses 10s."`
}

type Firecracker struct {
	Binary          string            `json:"binary"`
	StartupTimeout  time.Duration     `json:"startupTimeout"`
	ShutdownTimeout time.Duration     `json:"shutdownTimeout"`
	CPUs            int               `json:"cpus"`
	MemoryMiB       units.MiB         `json:"memoryMiB"`
	Kernel          KernelInput       `json:"kernel"`
	Disks           []ImageMountInput `json:"disks,omitempty"`
}

func (d Document) firecrackerManifest() (*Manifest, error) {
	ssh := d.SSH
	if d.decoded {
		defaults := DefaultDocument().SSH
		if ssh.User == defaults.User {
			ssh.User = ""
		}
		if ssh.RetryDelay == defaults.RetryDelay {
			ssh.RetryDelay = 0
		}
	}
	if d.explicitSSH || !reflect.DeepEqual(ssh, SSHInput{}) {
		return nil, fmt.Errorf("firecracker: manifest.ssh settings require an unsupported guest control transport")
	}
	if d.explicitVSock || d.VSock != (VSockInput{}) {
		return nil, fmt.Errorf("firecracker: manifest.vsock settings are unsupported")
	}
	// These fields configure QEMU devices or process policy and must never
	// be silently accepted by another backend. Duration defaults are seeded
	// by decoding; allow the same defaults for documents constructed in Go.
	qemu := d.QEMU
	qemu.GuestDefaultTimeout, qemu.ShutdownTimeout = 0, 0
	defaults := DefaultDocument().QEMU
	defaults.GuestDefaultTimeout, defaults.ShutdownTimeout = 0, 0
	if !reflect.DeepEqual(mergeQEMUInput(defaults, qemu), defaults) ||
		(d.QEMU.GuestDefaultTimeout != 0 && d.QEMU.GuestDefaultTimeout != DefaultDocument().QEMU.GuestDefaultTimeout) ||
		(d.QEMU.ShutdownTimeout != 0 && d.QEMU.ShutdownTimeout != DefaultDocument().QEMU.ShutdownTimeout) {
		return nil, fmt.Errorf("firecracker does not support manifest.qemu settings")
	}
	d = DocumentWithDefaults(d)
	if d.Machine.Type != "microvm" || d.Machine.CPU != "" || d.Machine.ID != "" {
		return nil, fmt.Errorf("firecracker does not support QEMU machine type, cpu or id settings")
	}
	if d.Graphics != nil && d.Graphics.Backend != "headless" {
		return nil, fmt.Errorf("firecracker does not support graphics")
	}
	if d.Kernel.Path == "" {
		return nil, fmt.Errorf("manifest.kernel.path is required")
	}
	if d.Machine.VCPU < 0 || d.Machine.VCPU > 32 {
		return nil, fmt.Errorf("manifest.machine.vcpu must be between 1 and 32 (zero selects 1)")
	}
	if d.Machine.Memory <= 0 || d.Machine.Memory > 1048576 {
		return nil, fmt.Errorf("manifest.machine.memory must be between 1 and 1048576 MiB")
	}
	if d.Machine.KVM != nil && !*d.Machine.KVM {
		return nil, fmt.Errorf("firecracker requires KVM")
	}
	if len(d.Networks) != 0 {
		return nil, fmt.Errorf("firecracker: manifest.networks is unsupported")
	}
	if len(d.Mounts) != len(d.Mounts.Image()) {
		return nil, fmt.Errorf("firecracker supports only image mounts")
	}
	if len(d.WriteFiles) != 0 || d.Workspace != (WorkspaceInput{}) {
		return nil, fmt.Errorf("firecracker: guest file, workspace and SSH configuration requires an unsupported guest control transport")
	}
	if len(d.Run) != 0 || len(d.Notifications.Exec) != 0 || d.Balloon != nil || len(d.Hotplug.Mounts) != 0 || len(d.Hotplug.Networks) != 0 {
		return nil, fmt.Errorf("firecracker: run, notifications, balloon and hotplug configuration is unsupported")
	}
	if d.Kernel.Serial != KernelSerialOff && d.Kernel.Serial != KernelSerialPrint {
		return nil, fmt.Errorf("firecracker kernel.serial must be off or print")
	}
	fc := &Firecracker{Binary: d.Firecracker.Binary, CPUs: d.Machine.VCPU, MemoryMiB: d.Machine.Memory, Kernel: d.Kernel, Disks: d.Mounts.Image(), StartupTimeout: d.Firecracker.StartupTimeout.Duration(), ShutdownTimeout: d.Firecracker.ShutdownTimeout.Duration()}
	if fc.Binary == "" {
		fc.Binary = "firecracker"
	}
	if fc.CPUs == 0 {
		fc.CPUs = 1
	}
	if fc.StartupTimeout < 0 || fc.ShutdownTimeout < 0 {
		return nil, fmt.Errorf("firecracker timeouts must not be negative")
	}
	if fc.StartupTimeout == 0 {
		fc.StartupTimeout = 10 * time.Second
	}
	if fc.ShutdownTimeout == 0 {
		fc.ShutdownTimeout = 10 * time.Second
	}
	if err := d.ResolveWorkingDir(); err != nil {
		return nil, err
	}
	resolve := func(path string) string {
		if path == "" || filepath.IsAbs(path) {
			return path
		}
		return filepath.Join(d.WorkingDir, path)
	}
	fc.Kernel.Path, fc.Kernel.InitrdPath = resolve(fc.Kernel.Path), resolve(fc.Kernel.InitrdPath)
	for _, path := range []string{fc.Binary, fc.Kernel.Path, fc.Kernel.InitrdPath, d.WorkingDir, d.StateDir} {
		if strings.ContainsRune(path, '\x00') {
			return nil, fmt.Errorf("firecracker path contains NUL")
		}
	}
	if strings.ContainsRune(strings.Join(fc.Kernel.Params, " "), '\x00') {
		return nil, fmt.Errorf("firecracker kernel.params contains NUL")
	}
	for i := range fc.Disks {
		disk := &fc.Disks[i]
		if disk.SourcePath == "" {
			return nil, fmt.Errorf("firecracker mounts[%d].source is required", i)
		}
		if disk.Image.Format != "" && disk.Image.Format != "raw" {
			return nil, fmt.Errorf("firecracker mounts[%d] requires raw disk format", i)
		}
		if disk.Image.AutoCreate {
			return nil, fmt.Errorf("firecracker mounts[%d]: image.create is unsupported; provide an existing raw disk", i)
		}
		if disk.Image.Size != 0 || disk.Image.FSType != "" || disk.Image.Label != nil || disk.Image.Serial != nil || disk.Image.Direct {
			return nil, fmt.Errorf("firecracker mounts[%d]: QEMU image creation, cache and serial settings are unsupported", i)
		}
		if strings.ContainsRune(disk.SourcePath, '\x00') {
			return nil, fmt.Errorf("firecracker disk path contains NUL")
		}
		disk.SourcePath = resolve(disk.SourcePath)
		disk.Image.Format = "raw"
	}
	return &Manifest{Backend: "firecracker", Firecracker: fc, Identity: Identity{HostName: d.HostName}, Paths: Paths{WorkingDir: d.WorkingDir, RuntimeDir: RuntimeDir{Mode: RuntimeDirPath, Path: d.StateDir}}, Persistence: Persistence{StateDir: d.StateDir, BaseDir: d.StateDir}}, nil
}
