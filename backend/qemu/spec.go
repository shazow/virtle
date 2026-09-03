package qemu

import (
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	imanifest "github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/units"
	"github.com/shazow/virtle/vm"
)

// specDocument lowers a neutral vm.Spec plus the Backend configuration onto the
// internal manifest document, which then flows through the same defaults,
// validation, and resolution pipeline as a TOML manifest. When base is
// non-nil (a manifest.Load-configured backend), the spec overlays it:
// scalar fields override, and Shares/Disks/Ports/Files replace the neutral
// entries represented by the loaded Spec.
func specDocument(spec *vm.Spec, cfg *Backend, base *imanifest.Document) (imanifest.Document, error) {
	if spec == nil {
		spec = &vm.Spec{}
	}
	doc := imanifest.DefaultDocument()
	if base != nil {
		// The overlay below writes into the network and machine-option
		// collections, so detach them from the backend's stored document.
		doc = *base
		doc.Networks = slices.Clone(doc.Networks)
		doc.QEMU.MachineOptions = maps.Clone(doc.QEMU.MachineOptions)
	}

	if cfg.HostName != "" {
		doc.HostName = cfg.HostName
	}

	// Spec.Dir wins; a Go-configured backend without one gets a fresh
	// temporary directory, a manifest.Load backend keeps the manifest's.
	dir := spec.Dir
	if dir == "" {
		if base == nil {
			tmp, err := os.MkdirTemp("", "virtle-")
			if err != nil {
				return imanifest.Document{}, fmt.Errorf("create working directory: %w", err)
			}
			dir = tmp
		} else {
			dir = doc.WorkingDir
		}
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return imanifest.Document{}, fmt.Errorf("resolve working directory %q: %w", dir, err)
	}
	doc.WorkingDir = abs

	cpus := spec.CPUs
	if cpus == 0 && base == nil && doc.Machine.VCPU == 0 {
		cpus = runtime.NumCPU()
	}
	if cpus != 0 {
		doc.Machine.VCPU = cpus
	}

	memory := spec.Memory
	if memory == 0 && base == nil {
		memory = DefaultMemory
	}
	if memory != 0 {
		if memory%units.Mebibyte != 0 {
			return imanifest.Document{}, fmt.Errorf("memory size %s is not MiB-aligned", memory)
		}
		doc.Machine.Memory = memory.Mebibytes()
	}

	if spec.Kernel != (vm.Kernel{}) {
		doc.Kernel.Path = spec.Kernel.Path
		doc.Kernel.InitrdPath = spec.Kernel.Initrd
		doc.Kernel.Params = strings.Fields(spec.Kernel.Cmdline)
	}
	if doc.Kernel.Path == "" {
		return imanifest.Document{}, fmt.Errorf("the qemu backend requires a direct kernel boot source (vm.Spec.Kernel)")
	}

	// Backend knobs.
	if cfg.Binary != "" || len(cfg.ExtraArgs) > 0 {
		binary := cfg.Binary
		if binary == "" && len(doc.QEMU.Exec) > 0 {
			binary = doc.QEMU.Exec[0]
		}
		exec := []string{}
		if binary != "" {
			exec = append(exec, binary)
		}
		doc.QEMU.Exec = append(exec, cfg.ExtraArgs...)
	}
	if cfg.MachineType != "" {
		doc.Machine.Type = cfg.MachineType
	}
	if len(cfg.MachineOptions) > 0 {
		if doc.QEMU.MachineOptions == nil {
			doc.QEMU.MachineOptions = map[string]string{}
		}
		for k, v := range cfg.MachineOptions {
			doc.QEMU.MachineOptions[k] = v
		}
	}
	if cfg.CPUModel != "" {
		doc.Machine.CPU = cfg.CPUModel
	}
	switch cfg.Accel {
	case AccelAuto:
	case AccelKVM:
		enabled := true
		doc.Machine.KVM = &enabled
	case AccelTCG:
		enabled := false
		doc.Machine.KVM = &enabled
		if doc.Machine.CPU == "" {
			doc.Machine.CPU = "max"
			if doc.Host.System == "x86_64-linux" || (doc.Host.System == "" && runtime.GOARCH == "amd64") {
				doc.Machine.CPU += ",+x2apic"
			}
		}
	default:
		return imanifest.Document{}, fmt.Errorf("unsupported QEMU accelerator %q", cfg.Accel)
	}
	if cfg.Console != "" {
		doc.Kernel.Serial = string(cfg.Console)
	}
	if cfg.Seccomp {
		doc.QEMU.Seccomp = true
	}
	if cfg.Balloon {
		doc.Balloon = &imanifest.BalloonInput{Enabled: true}
	}
	if qga, ok := cfg.RemoteControl.(QGA); ok && qga.SocketPath != "" {
		doc.QEMU.GuestAgentSocket = qga.SocketPath
	}
	if cfg.HotplugPorts > doc.QEMU.HotplugPorts {
		doc.QEMU.HotplugPorts = cfg.HotplugPorts
	}

	if err := applySpecDevices(&doc, spec); err != nil {
		return imanifest.Document{}, err
	}
	return doc, nil
}

// applySpecDevices replaces the document entries represented by the neutral
// Spec, in slice order. Backend-only entries remain in place; extra Spec
// entries append, and omitted Spec entries are removed.
func applySpecDevices(doc *imanifest.Document, spec *vm.Spec) error {
	mounts, err := overlaySpecMounts(doc.Mounts, spec.Shares, spec.Disks)
	if err != nil {
		return err
	}
	doc.Mounts = mounts
	if err := overlaySpecPorts(doc, spec.Ports); err != nil {
		return err
	}
	files, err := overlaySpecFiles(doc.WriteFiles, spec.Files)
	if err != nil {
		return err
	}
	doc.WriteFiles = files
	return nil
}

func overlaySpecMounts(mounts imanifest.MountsInput, shares []vm.Share, disks []vm.Disk) (imanifest.MountsInput, error) {
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
				overlaid, err := overlayDisk(input, disks[diskIndex])
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
		overlaid, err := overlayDisk(imanifest.ImageMountInput{}, disks[diskIndex])
		if err != nil {
			return nil, err
		}
		result = append(result, overlaid)
	}
	return result, nil
}

func overlayShare(input imanifest.VirtioFSMountInput, share vm.Share) (imanifest.VirtioFSMountInput, error) {
	if share.Tag == "" {
		return imanifest.VirtioFSMountInput{}, fmt.Errorf("share for %q: Tag is required", share.HostPath)
	}
	input.Type = imanifest.MountTypeVirtioFS
	input.Tag = share.Tag
	input.SourcePath = share.HostPath
	input.ReadOnly = share.ReadOnly
	input.Target = share.GuestPath
	if input.VirtioFS.Socket == "" {
		input.VirtioFS.Socket = share.Tag + ".sock"
		input.VirtioFS.Bin = "virtiofsd"
	}
	return input, nil
}

func overlayDisk(input imanifest.ImageMountInput, disk vm.Disk) (imanifest.ImageMountInput, error) {
	if disk.Path == "" {
		return imanifest.ImageMountInput{}, fmt.Errorf("disk: Path is required")
	}
	if disk.Size != 0 && disk.Size%units.Mebibyte != 0 {
		return imanifest.ImageMountInput{}, fmt.Errorf("disk %q: size %s is not MiB-aligned", disk.Path, disk.Size)
	}
	input.Type = imanifest.MountTypeImage
	input.SourcePath = disk.Path
	input.Image.Size = disk.Size.Mebibytes()
	input.Image.Format = disk.Format
	input.Image.AutoCreate = disk.Size != 0
	return input, nil
}

func overlaySpecPorts(doc *imanifest.Document, ports []vm.Forward) error {
	portIndex := 0
	for i := range doc.Networks {
		forwards := make([]imanifest.ForwardPort, 0, len(doc.Networks[i].Forward))
		for _, forward := range doc.Networks[i].Forward {
			if forward.From != "" && forward.From != "host" {
				forwards = append(forwards, forward)
				continue
			}
			if portIndex < len(ports) {
				forwards = append(forwards, specForward(ports[portIndex]))
			}
			portIndex++
		}
		doc.Networks[i].Forward = forwards
	}
	if portIndex < len(ports) {
		if len(doc.Networks) == 0 {
			return fmt.Errorf("port forwards require a network device")
		}
		for ; portIndex < len(ports); portIndex++ {
			doc.Networks[0].Forward = append(doc.Networks[0].Forward, specForward(ports[portIndex]))
		}
	}
	return nil
}

func specForward(forward vm.Forward) imanifest.ForwardPort {
	proto := forward.Proto
	if proto == "" {
		proto = vm.TCP
	}
	return imanifest.ForwardPort{Proto: string(proto), From: "host", Host: forward.HostAddr, Guest: forward.GuestAddr}
}

func overlaySpecFiles(inputs []imanifest.WriteFileInput, files []vm.File) ([]imanifest.WriteFileInput, error) {
	result := make([]imanifest.WriteFileInput, 0, len(inputs)+len(files))
	fileIndex := 0
	for _, input := range inputs {
		if input.Text == nil {
			result = append(result, input)
			continue
		}
		if fileIndex < len(files) {
			overlaid, err := overlayFile(input, files[fileIndex])
			if err != nil {
				return nil, err
			}
			result = append(result, overlaid)
		}
		fileIndex++
	}
	for ; fileIndex < len(files); fileIndex++ {
		overlaid, err := overlayFile(imanifest.WriteFileInput{}, files[fileIndex])
		if err != nil {
			return nil, err
		}
		result = append(result, overlaid)
	}
	return result, nil
}

func overlayFile(input imanifest.WriteFileInput, file vm.File) (imanifest.WriteFileInput, error) {
	if file.GuestPath == "" {
		return imanifest.WriteFileInput{}, fmt.Errorf("file: GuestPath is required")
	}
	if file.Content == nil {
		return imanifest.WriteFileInput{}, fmt.Errorf("file %q: Content is required", file.GuestPath)
	}
	data, err := io.ReadAll(file.Content)
	if err != nil {
		return imanifest.WriteFileInput{}, fmt.Errorf("read content for guest file %q: %w", file.GuestPath, err)
	}
	text := string(data)
	input.GuestPath = file.GuestPath
	input.Text = &text
	input.Path = nil
	input.Mode = nil
	if file.Mode != 0 {
		mode := fmt.Sprintf("%03o", file.Mode.Perm())
		input.Mode = &mode
	}
	return input, nil
}
