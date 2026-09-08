package firecracker

import (
	"fmt"
	"path/filepath"

	imanifest "github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/units"
	"github.com/shazow/virtle/vm"
)

func (b *Backend) resolveSpec(spec *vm.Spec) (*imanifest.Manifest, error) {
	if spec == nil {
		spec = &vm.Spec{}
	}
	if len(spec.Files)+len(spec.Shares)+len(spec.Ports) != 0 {
		return nil, fmt.Errorf("firecracker: guest files, shares and port forwarding are unsupported")
	}
	doc := imanifest.Document{Backend: "firecracker"}
	if b.doc != nil {
		doc = *b.doc
	}
	if spec.Dir != "" {
		doc.WorkingDir = spec.Dir
	}
	if spec.CPUs != 0 {
		doc.Machine.VCPU = spec.CPUs
	}
	if spec.Memory != 0 {
		if spec.Memory%units.Mebibyte != 0 {
			return nil, fmt.Errorf("firecracker memory must be MiB-aligned")
		}
		doc.Machine.Memory = spec.Memory.Mebibytes()
	}
	if spec.Kernel != (vm.Kernel{}) {
		doc.Kernel.Path, doc.Kernel.InitrdPath = spec.Kernel.Path, spec.Kernel.Initrd
		// Preserve quotes and whitespace in a caller's kernel command line.
		doc.Kernel.Params = []string{spec.Kernel.Cmdline}
	}
	doc.Mounts = make(imanifest.MountsInput, 0, len(spec.Disks))
	for _, disk := range spec.Disks {
		if disk.Size != 0 || disk.GuestPath != "" {
			return nil, fmt.Errorf("firecracker disk creation and automatic guest mounts are unsupported")
		}
		doc.Mounts = append(doc.Mounts, imanifest.ImageMountInput{Type: "image", SourcePath: disk.Path, ReadOnly: disk.ReadOnly, Image: imanifest.ImageInput{Format: disk.Format}})
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
		doc.Kernel.Serial = b.Console
	}
	mf, err := doc.Manifest()
	if err != nil {
		return nil, err
	}
	// Executable paths containing a directory resolve like image paths.
	if filepath.Base(mf.Firecracker.Binary) != mf.Firecracker.Binary && !filepath.IsAbs(mf.Firecracker.Binary) {
		mf.Firecracker.Binary = filepath.Join(mf.Paths.WorkingDir, mf.Firecracker.Binary)
	}
	return mf, nil
}
