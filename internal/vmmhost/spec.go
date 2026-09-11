package vmmhost

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/internal/diskimage"
	imanifest "github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/units"
	"github.com/shazow/virtle/vm"
)

// SpecOptions steer ApplySpec for one backend.
type SpecOptions struct {
	Backend       string      // prefixes errors
	MaxCPUs       int         // the VMM's vCPU bound
	DefaultMemory units.Bytes // guest memory for a document built from the Spec alone when it sets none
	Loaded        bool        // doc came from a manifest: its kernel and memory stand where the Spec is silent
	Shares        bool        // Spec.Shares become virtiofs mounts instead of failing with errors.ErrUnsupported
	HostName      string      // replaces the document's VM name, when set
	StateDir      string      // replaces the document's state directory, when set
}

// ApplySpec lowers a neutral vm.Spec onto doc the way the microVM backends
// share: identity and directories, machine size, kernel, and mounts. Spec
// disks and shares overlay the document's image and virtiofs mounts by
// position, so manifest-only settings such as image.label survive a
// manifest.Load, and omitted entries are removed, as on QEMU. Spec features
// that need a guest transport, and shares on a VMM without them, fail with
// errors.ErrUnsupported rather than being dropped. The Spec is validated in
// its own vocabulary first; manifest resolution repeats the checks in
// manifest terms.
func ApplySpec(doc *imanifest.Document, spec *vm.Spec, o SpecOptions) error {
	if len(spec.Files) != 0 {
		return fmt.Errorf("%s: guest files (vm.Spec.Files) need a guest control transport: %w", o.Backend, errors.ErrUnsupported)
	}
	if len(spec.Shares) != 0 && !o.Shares {
		return fmt.Errorf("%s: host shares (vm.Spec.Shares): %w", o.Backend, errors.ErrUnsupported)
	}
	if len(spec.Ports) != 0 {
		return fmt.Errorf("%s: port forwards (vm.Spec.Ports): %w", o.Backend, errors.ErrUnsupported)
	}
	if !o.Loaded && spec.Kernel.Path == "" {
		return fmt.Errorf("%s: vm.Spec.Kernel.Path is required", o.Backend)
	}
	if spec.CPUs < 0 || spec.CPUs > o.MaxCPUs {
		return fmt.Errorf("%s: vm.Spec.CPUs must be between 1 and %d, got %d", o.Backend, o.MaxCPUs, spec.CPUs)
	}
	if o.HostName != "" {
		doc.HostName = o.HostName
	}
	if spec.Dir != "" {
		doc.WorkingDir = spec.Dir
	}
	if o.StateDir != "" {
		doc.StateDir = o.StateDir
	}
	// A zero CPU count is derived by manifest resolution (every host CPU).
	if spec.CPUs != 0 {
		doc.Machine.VCPU = spec.CPUs
	}
	memory := spec.Memory
	if memory == 0 && !o.Loaded {
		memory = o.DefaultMemory
	}
	if memory != 0 {
		if memory%units.Mebibyte != 0 {
			return fmt.Errorf("memory size %s is not MiB-aligned", memory)
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
	mounts, err := overlayMounts(o.Backend, doc.Mounts, spec.Shares, spec.Disks)
	if err != nil {
		return err
	}
	doc.Mounts = mounts
	return nil
}

// overlayMounts replaces the document's virtiofs and image mounts with the
// Spec's shares and disks, each kind in slice order: extra Spec entries
// append, omitted ones are removed, other kinds stay for manifest resolution
// to judge.
func overlayMounts(backend string, mounts imanifest.MountsInput, shares []vm.Share, disks []vm.Disk) (imanifest.MountsInput, error) {
	result := make(imanifest.MountsInput, 0, len(mounts)+len(shares)+len(disks))
	shareIndex, diskIndex := 0, 0
	for _, mount := range mounts {
		switch input := mount.(type) {
		case imanifest.VirtioFSMountInput:
			if shareIndex < len(shares) {
				overlaid, err := overlayShare(input, shares[shareIndex])
				if err != nil {
					return nil, err
				}
				result = append(result, overlaid)
			}
			shareIndex++
		case imanifest.ImageMountInput:
			if diskIndex < len(disks) {
				overlaid, err := overlayDisk(backend, input, disks[diskIndex])
				if err != nil {
					return nil, err
				}
				result = append(result, overlaid)
			}
			diskIndex++
		default:
			result = append(result, mount)
		}
	}
	for ; shareIndex < len(shares); shareIndex++ {
		overlaid, err := overlayShare(imanifest.VirtioFSMountInput{}, shares[shareIndex])
		if err != nil {
			return nil, err
		}
		result = append(result, overlaid)
	}
	for ; diskIndex < len(disks); diskIndex++ {
		overlaid, err := overlayDisk(backend, imanifest.ImageMountInput{}, disks[diskIndex])
		if err != nil {
			return nil, err
		}
		result = append(result, overlaid)
	}
	return result, nil
}

// overlayShare keeps the mount's virtiofs daemon settings (socket, bin,
// args); manifest resolution defaults the missing ones.
func overlayShare(input imanifest.VirtioFSMountInput, share vm.Share) (imanifest.VirtioFSMountInput, error) {
	if share.Tag == "" {
		return imanifest.VirtioFSMountInput{}, fmt.Errorf("share for %q: Tag is required", share.HostPath)
	}
	input.Type = imanifest.MountTypeVirtioFS
	input.Tag, input.SourcePath, input.ReadOnly, input.Target = share.Tag, share.HostPath, share.ReadOnly, share.GuestPath
	return input, nil
}

func overlayDisk(backend string, input imanifest.ImageMountInput, disk vm.Disk) (imanifest.ImageMountInput, error) {
	// "/" names the root device, which the kernel mounts itself; any other
	// mount point needs an agent in the guest.
	if disk.GuestPath != "" && disk.GuestPath != "/" {
		return imanifest.ImageMountInput{}, fmt.Errorf("%s: disk %q: guest mounting at %q (vm.Disk.GuestPath) needs a guest control transport: %w", backend, disk.Path, disk.GuestPath, errors.ErrUnsupported)
	}
	if disk.Format != "" && disk.Format != "raw" {
		return imanifest.ImageMountInput{}, fmt.Errorf("%s: disk %q: Format %q is not supported, only raw images are: %w", backend, disk.Path, disk.Format, errors.ErrUnsupported)
	}
	if disk.Size%units.Mebibyte != 0 {
		return imanifest.ImageMountInput{}, fmt.Errorf("%s: disk %q: size %s is not MiB-aligned", backend, disk.Path, disk.Size)
	}
	input.Type = imanifest.MountTypeImage
	input.SourcePath, input.Target, input.ReadOnly = disk.Path, disk.GuestPath, disk.ReadOnly
	// As on QEMU, a size asks for the image to be created when missing.
	input.Image.Format, input.Image.Size, input.Image.AutoCreate = disk.Format, disk.Size.Mebibytes(), disk.Size != 0
	return input, nil
}

// ApplyTAP makes every declared network (or one default entry) a tap
// network on device, for a Backend.Link naming a host TAP.
func ApplyTAP(doc *imanifest.Document, device string) {
	// The overlay writes into the network entries, so detach them from the
	// backend's stored document.
	doc.Networks = slices.Clone(doc.Networks)
	if len(doc.Networks) == 0 {
		doc.Networks = []imanifest.NetworkInput{{}}
	}
	for i := range doc.Networks {
		doc.Networks[i].Type = imanifest.NetworkTypeTAP
		doc.Networks[i].Tap = device
	}
}

// CreateDisks formats the images vm.Disk.Size asked for before launch, as
// QEMU does; existing images are kept.
func CreateDisks(disks []imanifest.RawDisk, logger *slog.Logger) error {
	for _, disk := range disks {
		if !disk.Create {
			continue
		}
		created, err := diskimage.Ensure(diskimage.Image{Path: disk.Path, Size: disk.SizeMiB.Bytes().Int64(), Label: disk.Label})
		if err != nil {
			return err
		}
		if created {
			logger.Info("created disk image", "path", disk.Path, "size_mib", disk.SizeMiB)
		}
	}
	return nil
}

// NetworkStatuses lists TAP NICs for Status. The host kernel networks them,
// so none is attached to a network virtle runs and no address is known.
func NetworkStatuses(networks []imanifest.TapNetwork) []backend.NetworkStatus {
	if len(networks) == 0 {
		return nil
	}
	statuses := make([]backend.NetworkStatus, 0, len(networks))
	for _, network := range networks {
		statuses = append(statuses, backend.NetworkStatus{ID: network.ID, MAC: network.MAC})
	}
	return statuses
}
