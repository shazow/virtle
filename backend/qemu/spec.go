package qemu

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	imanifest "github.com/shazow/virtle/internal/manifest"
	iunits "github.com/shazow/virtle/internal/units"
	"github.com/shazow/virtle/units"
	"github.com/shazow/virtle/vm"
)

// specDocument lowers a neutral vm.Spec plus the backend Config onto the
// internal manifest document, which then flows through the same defaults,
// validation, and resolution pipeline as a TOML manifest. When base is
// non-nil (a manifest.Load-configured backend), the spec overlays it:
// scalar fields override, and Shares/Disks/Ports/Files entries not already
// present are appended.
func specDocument(spec *vm.Spec, cfg Config, base *imanifest.Document) (imanifest.Document, error) {
	if spec == nil {
		spec = &vm.Spec{}
	}
	doc := imanifest.DefaultDocument()
	if base != nil {
		doc = *base
	}

	if cfg.HostName != "" {
		doc.HostName = cfg.HostName
	}

	dir := spec.Dir
	if dir == "" && base == nil {
		tmp, err := os.MkdirTemp("", "virtle-")
		if err != nil {
			return imanifest.Document{}, fmt.Errorf("create working directory: %w", err)
		}
		dir = tmp
	}
	if dir != "" {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return imanifest.Document{}, fmt.Errorf("resolve working directory %q: %w", dir, err)
		}
		doc.WorkingDir = abs
	}
	if !filepath.IsAbs(doc.WorkingDir) {
		abs, err := filepath.Abs(doc.WorkingDir)
		if err != nil {
			return imanifest.Document{}, fmt.Errorf("resolve working directory %q: %w", doc.WorkingDir, err)
		}
		doc.WorkingDir = abs
	}

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
		doc.Machine.Memory = iunits.MiB(memory.Mebibytes())
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
	if cfg.Machine != "" {
		doc.Machine.Type = cfg.Machine
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
	if cfg.KVM != nil {
		kvm := *cfg.KVM
		doc.Machine.KVM = &kvm
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

// applySpecDevices appends Shares, Disks, Ports, and Files from the spec
// that the document does not already carry (matched by tag/path), so a
// spec produced by manifest.Load overlays cleanly instead of duplicating.
func applySpecDevices(doc *imanifest.Document, spec *vm.Spec) error {
	haveShare := map[string]bool{}
	for _, m := range doc.Mounts.VirtioFS() {
		haveShare[m.Tag] = true
	}
	for _, share := range spec.Shares {
		if share.Tag == "" {
			return fmt.Errorf("share for %q: Tag is required", share.HostPath)
		}
		if haveShare[share.Tag] {
			continue
		}
		doc.Mounts = append(doc.Mounts, imanifest.VirtioFSMountInput{
			Type: imanifest.MountTypeVirtioFS,
			MountInput: imanifest.MountInput{
				Tag:        share.Tag,
				SourcePath: share.HostPath,
				ReadOnly:   share.ReadOnly,
			},
			Target: share.GuestPath,
			// The socket lives under the state dir; the backend launches
			// virtiofsd for it, matching hotplug's <tag>.sock convention.
			VirtioFS: imanifest.VirtioFSInput{Socket: share.Tag + ".sock"},
		})
	}

	haveDisk := map[string]bool{}
	for _, m := range doc.Mounts.Image() {
		haveDisk[m.SourcePath] = true
	}
	for _, disk := range spec.Disks {
		if disk.Path == "" {
			return fmt.Errorf("disk: Path is required")
		}
		if haveDisk[disk.Path] {
			continue
		}
		if disk.Size != 0 && disk.Size%units.Mebibyte != 0 {
			return fmt.Errorf("disk %q: size %s is not MiB-aligned", disk.Path, disk.Size)
		}
		doc.Mounts = append(doc.Mounts, imanifest.ImageMountInput{
			Type:       imanifest.MountTypeImage,
			SourcePath: disk.Path,
			Image: imanifest.ImageInput{
				Size:       iunits.MiB(disk.Size.Mebibytes()),
				Format:     disk.Format,
				AutoCreate: disk.Size != 0,
			},
		})
	}

	if len(spec.Ports) > 0 {
		if len(doc.Networks) == 0 {
			return fmt.Errorf("port forwards require a network device")
		}
		net := &doc.Networks[0]
		havePort := map[string]bool{}
		for _, f := range net.Forward {
			havePort[f.Proto+"|"+f.Host+"|"+f.Guest] = true
		}
		for _, fwd := range spec.Ports {
			proto := fwd.Proto
			if proto == "" {
				proto = "tcp"
			}
			if havePort[proto+"|"+fwd.HostAddr+"|"+fwd.GuestAddr] {
				continue
			}
			net.Forward = append(net.Forward, imanifest.ForwardPort{
				Proto: proto,
				From:  "host",
				Host:  fwd.HostAddr,
				Guest: fwd.GuestAddr,
			})
		}
	}

	haveFile := map[string]bool{}
	for _, f := range doc.WriteFiles {
		haveFile[f.GuestPath] = true
	}
	for _, file := range spec.Files {
		if file.GuestPath == "" {
			return fmt.Errorf("file: GuestPath is required")
		}
		if haveFile[file.GuestPath] {
			continue
		}
		if file.Content == nil {
			return fmt.Errorf("file %q: Content is required", file.GuestPath)
		}
		data, err := io.ReadAll(file.Content)
		if err != nil {
			return fmt.Errorf("read content for guest file %q: %w", file.GuestPath, err)
		}
		text := string(data)
		input := imanifest.WriteFileInput{
			GuestPath: file.GuestPath,
			Text:      &text,
		}
		if file.Mode != 0 {
			mode := fmt.Sprintf("%03o", file.Mode.Perm())
			input.Mode = &mode
		}
		doc.WriteFiles = append(doc.WriteFiles, input)
	}
	return nil
}
