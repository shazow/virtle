package qemu

import (
	"fmt"

	"github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/units"
	"github.com/shazow/virtle/vm"
)

// defaultVirtiofsdBinary is the daemon started for a share when Config names
// no other one.
const defaultVirtiofsdBinary = "virtiofsd"

// document lowers a neutral spec plus this backend's configuration onto the
// manifest document virtle already knows how to resolve. Doing the lowering
// here — rather than teaching the resolver about vm.Spec — keeps one set of
// defaulting and validation rules behind both the CLI and the library.
func (c Config) document(spec *vm.Spec) (manifest.Document, error) {
	if spec == nil {
		return manifest.Document{}, fmt.Errorf("spec is required")
	}

	doc := manifest.NewDocument()
	if c.HostName != "" {
		doc.HostName = c.HostName
	}
	if spec.Dir != "" {
		doc.StateDir = spec.Dir
	}

	doc.QEMU.Exec = append([]string{c.Binary}, c.ExtraArgs...)
	doc.QEMU.Seccomp = c.Seccomp
	doc.QEMU.MachineOptions = c.MachineOptions

	doc.Machine.Type = c.Machine
	doc.Machine.CPU = c.CPU
	doc.Machine.KVM = c.KVM
	doc.Machine.VCPU = spec.CPUs
	if spec.Memory != 0 {
		memory, err := units.BytesToMiB(spec.Memory)
		if err != nil {
			return manifest.Document{}, fmt.Errorf("spec.Memory: %w", err)
		}
		doc.Machine.Memory = memory
	}

	doc.Kernel.Path = spec.Kernel.Path
	doc.Kernel.InitrdPath = spec.Kernel.Initrd
	if spec.Kernel.Cmdline != "" {
		doc.Kernel.Params = []string{spec.Kernel.Cmdline}
	}
	if c.Console != "" {
		doc.Kernel.Serial = string(c.Console)
	}

	if c.Balloon {
		doc.Balloon = &manifest.BalloonInput{Enabled: true}
	}
	if c.CIDRange != (CIDRange{}) {
		doc.VSock.CIDRange = manifest.RangeInput{Min: c.CIDRange.Min, Max: c.CIDRange.Max}
	}

	mounts, err := c.lowerMounts(spec)
	if err != nil {
		return manifest.Document{}, err
	}
	doc.Mounts = mounts

	networks, err := lowerPorts(spec.Ports)
	if err != nil {
		return manifest.Document{}, err
	}
	doc.Networks = networks

	return doc, nil
}

func (c Config) lowerMounts(spec *vm.Spec) (manifest.MountsInput, error) {
	if len(spec.Shares)+len(spec.Disks) == 0 {
		return nil, nil
	}
	// An empty bin marks a share's socket as externally managed, so the
	// backend names the daemon it intends to start.
	virtiofsd := c.VirtiofsdBinary
	if virtiofsd == "" {
		virtiofsd = defaultVirtiofsdBinary
	}
	mounts := make(manifest.MountsInput, 0, len(spec.Shares)+len(spec.Disks))
	for i, share := range spec.Shares {
		if share.Tag == "" {
			return nil, fmt.Errorf("spec.Shares[%d].Tag is required", i)
		}
		if share.HostPath == "" {
			return nil, fmt.Errorf("spec.Shares[%d].HostPath is required", i)
		}
		mounts = append(mounts, manifest.VirtioFSMountInput{
			Type: manifest.MountTypeVirtioFS,
			MountInput: manifest.MountInput{
				Tag:        share.Tag,
				SourcePath: share.HostPath,
				ReadOnly:   share.ReadOnly,
			},
			Target: share.GuestPath,
			VirtioFS: manifest.VirtioFSInput{
				// Socket paths are relative to the state directory, so
				// concurrent machines with different Dirs never collide.
				Socket: share.Tag + ".sock",
				Bin:    virtiofsd,
				Args:   c.VirtiofsdArgs,
			},
		})
	}
	for i, disk := range spec.Disks {
		if disk.Path == "" {
			return nil, fmt.Errorf("spec.Disks[%d].Path is required", i)
		}
		size, err := units.BytesToMiB(disk.Size)
		if err != nil {
			return nil, fmt.Errorf("spec.Disks[%d].Size: %w", i, err)
		}
		mounts = append(mounts, manifest.ImageMountInput{
			Type:       manifest.MountTypeImage,
			SourcePath: disk.Path,
			ReadOnly:   disk.ReadOnly,
			Image: manifest.ImageInput{
				Size:   size,
				Format: disk.Format,
				// A sized disk is created on demand; an unsized one must
				// already exist.
				AutoCreate: size > 0,
			},
		})
	}
	return mounts, nil
}

// lowerPorts attaches the spec's forwards to virtle's default user-mode
// network, since that is the only network a Spec can currently describe.
func lowerPorts(ports []vm.Forward) ([]manifest.NetworkInput, error) {
	if len(ports) == 0 {
		return nil, nil
	}
	networks := manifest.DefaultDocument().Networks
	if len(networks) == 0 {
		return nil, fmt.Errorf("no default network to attach %d port forwards to", len(ports))
	}
	forwards := make([]manifest.ForwardPort, 0, len(ports))
	for i, port := range ports {
		if port.HostAddr == "" || port.GuestAddr == "" {
			return nil, fmt.Errorf("spec.Ports[%d] needs both HostAddr and GuestAddr", i)
		}
		proto := port.Proto
		if proto == "" {
			proto = "tcp"
		}
		from := "host"
		if port.FromGuest {
			from = "guest"
		}
		forwards = append(forwards, manifest.ForwardPort{
			Proto: proto,
			From:  from,
			Host:  port.HostAddr,
			Guest: port.GuestAddr,
		})
	}
	networks[0].Forward = forwards
	return networks, nil
}
