package firecracker

import (
	"errors"
	"fmt"

	imanifest "github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/units"
	"github.com/shazow/virtle/vm"
)

// resolveSpec lowers a neutral vm.Spec plus the Backend configuration onto the
// internal manifest document (overlaid on the loaded document for
// manifest.Load backends) and resolves it. A non-empty stateDir replaces the
// document's state directory. Spec features that need a guest transport or
// QEMU-only machinery are rejected with errors.ErrUnsupported rather than
// silently dropped.
func (b *Backend) resolveSpec(spec *vm.Spec, stateDir string) (*imanifest.Manifest, error) {
	if spec == nil {
		spec = &vm.Spec{}
	}
	if len(spec.Files) != 0 {
		return nil, fmt.Errorf("firecracker: guest files (vm.Spec.Files) need a guest control transport: %w", errors.ErrUnsupported)
	}
	if len(spec.Shares) != 0 {
		return nil, fmt.Errorf("firecracker: host shares (vm.Spec.Shares): %w", errors.ErrUnsupported)
	}
	if len(spec.Ports) != 0 {
		return nil, fmt.Errorf("firecracker: port forwards (vm.Spec.Ports): %w", errors.ErrUnsupported)
	}
	// Validate the Spec in its own vocabulary before lowering it; manifest
	// resolution repeats the checks in manifest terms for manifest.Load.
	switch b.Console {
	case "", ConsoleOff, ConsolePrint:
	default:
		return nil, fmt.Errorf("firecracker: Console %q is not ConsoleOff or ConsolePrint: %w", b.Console, errors.ErrUnsupported)
	}
	if b.doc == nil && spec.Kernel.Path == "" {
		return nil, fmt.Errorf("firecracker: vm.Spec.Kernel.Path is required")
	}
	if spec.CPUs < 0 || spec.CPUs > imanifest.MaxFirecrackerCPUs {
		return nil, fmt.Errorf("firecracker: vm.Spec.CPUs must be between 1 and %d, got %d", imanifest.MaxFirecrackerCPUs, spec.CPUs)
	}
	doc := imanifest.Document{Backend: imanifest.BackendFirecracker}
	if b.doc != nil {
		doc = *b.doc
	}
	if b.HostName != "" {
		doc.HostName = b.HostName
	}
	if spec.Dir != "" {
		doc.WorkingDir = spec.Dir
	}
	if stateDir != "" {
		doc.StateDir = stateDir
	}
	// A zero CPU count is derived by manifest resolution (every host CPU).
	if spec.CPUs != 0 {
		doc.Machine.VCPU = spec.CPUs
	}
	memory := spec.Memory
	if memory == 0 && b.doc == nil {
		memory = DefaultMemory
	}
	if memory != 0 {
		if memory%units.Mebibyte != 0 {
			return nil, fmt.Errorf("memory size %s is not MiB-aligned", memory)
		}
		doc.Machine.Memory = memory.Mebibytes()
	}
	if spec.Kernel != (vm.Kernel{}) {
		doc.Kernel.Path, doc.Kernel.InitrdPath = spec.Kernel.Path, spec.Kernel.Initrd
		// Preserve quotes and whitespace in a caller's kernel command line.
		doc.Kernel.Params = nil
		if spec.Kernel.Cmdline != "" {
			doc.Kernel.Params = []string{spec.Kernel.Cmdline}
		}
	}
	doc.Mounts = make(imanifest.MountsInput, 0, len(spec.Disks))
	for _, disk := range spec.Disks {
		if disk.Size != 0 {
			return nil, fmt.Errorf("firecracker: disk %q: image creation (vm.Disk.Size): %w", disk.Path, errors.ErrUnsupported)
		}
		if disk.GuestPath != "" {
			return nil, fmt.Errorf("firecracker: disk %q: guest mounting (vm.Disk.GuestPath) needs a guest control transport: %w", disk.Path, errors.ErrUnsupported)
		}
		if disk.Format != "" && disk.Format != "raw" {
			return nil, fmt.Errorf("firecracker: disk %q: Format %q is not supported, only raw images are: %w", disk.Path, disk.Format, errors.ErrUnsupported)
		}
		doc.Mounts = append(doc.Mounts, imanifest.ImageMountInput{Type: imanifest.MountTypeImage, SourcePath: disk.Path, ReadOnly: disk.ReadOnly, Image: imanifest.ImageInput{Format: disk.Format}})
	}
	if b.Binary != "" {
		doc.Firecracker.Binary = b.Binary
	}
	if b.StartupTimeout != 0 {
		doc.Firecracker.StartupTimeout = units.Duration(b.StartupTimeout)
	}
	if b.ShutdownTimeout != 0 {
		doc.Firecracker.ShutdownTimeout = units.Duration(b.ShutdownTimeout)
	}
	if b.Console != "" {
		doc.Kernel.Serial = string(b.Console)
	}
	mf, err := doc.Manifest()
	if err != nil {
		return nil, fmt.Errorf("resolve vm spec: %w", err)
	}
	return mf, nil
}
