// Package manifest loads virtle's TOML configuration into the neutral library
// types: a [vm.Spec] describing the machine, and the [backend.Backend]
// configured to start it.
//
// It is one loader among many, not the center of the API — a program can build
// a Spec in Go and never touch this package. What it buys is the manifest
// format the virtle CLI already speaks, with the same defaults and the same
// validation:
//
//	spec, b, err := manifest.Load(f)
//	inst, err := b.Start(ctx, spec)
//
// Scope: a manifest describes more than a VM. The sections that orchestrate a
// session rather than describe a machine — [run] host helper commands, [ssh],
// [notifications], and [hotplug] — stay with the CLI and are ignored here.
// Sections that do describe the machine but that the neutral Spec cannot yet
// express are reported as errors rather than dropped, so a loaded manifest
// never starts a machine quietly different from the one it describes.
//
// This package is experimental. The module is untagged v0 and the API may
// change until it settles.
package manifest

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/backend/qemu"
	internalmanifest "github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/vm"
)

// Load reads a virtle manifest and lowers it to a machine description plus the
// backend it configures. TOML and JSON are both accepted; the format is
// detected from the content.
//
// Host paths in the manifest are resolved the way the CLI resolves them, so a
// manifest read from another directory should be loaded with the process
// working directory set accordingly. File contents named by write_files are
// read at load time.
func Load(r io.Reader) (*vm.Spec, backend.Backend, error) {
	if r == nil {
		return nil, nil, fmt.Errorf("manifest reader is required")
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, nil, fmt.Errorf("read manifest: %w", err)
	}
	document, err := internalmanifest.DecodeDocumentBytes(data, "")
	if err != nil {
		return nil, nil, err
	}
	// Resolving applies defaults and runs validation. The resolved form is
	// discarded: what Start needs is rebuilt from the spec and config below,
	// so there is one lowering path rather than two.
	if _, err := document.Manifest(); err != nil {
		return nil, nil, err
	}

	document = internalmanifest.DocumentWithDefaults(document)
	if err := checkSupported(document); err != nil {
		return nil, nil, err
	}
	spec, err := loadSpec(document)
	if err != nil {
		return nil, nil, err
	}
	b, err := qemu.BackendWithQGA(loadConfig(document))
	if err != nil {
		return nil, nil, err
	}
	return spec, b, nil
}

func loadSpec(document internalmanifest.Document) (*vm.Spec, error) {
	spec := &vm.Spec{
		CPUs:   document.Machine.VCPU,
		Memory: document.Machine.Memory.Bytes(),
		Kernel: vm.Kernel{
			Path:    document.Kernel.Path,
			Initrd:  document.Kernel.InitrdPath,
			Cmdline: strings.Join(document.Kernel.Params, " "),
		},
		Dir: document.StateDir,
	}
	for _, mount := range document.Mounts.VirtioFS() {
		spec.Shares = append(spec.Shares, vm.Share{
			Tag:       mount.Tag,
			HostPath:  mount.SourcePath,
			GuestPath: mount.Target,
			ReadOnly:  mount.ReadOnly,
		})
	}
	for _, mount := range document.Mounts.Image() {
		spec.Disks = append(spec.Disks, vm.Disk{
			Path:     mount.SourcePath,
			Format:   mount.Image.Format,
			Size:     mount.Image.Size.Bytes(),
			ReadOnly: mount.ReadOnly,
		})
	}
	for _, network := range document.Networks {
		for _, forward := range network.Forward {
			spec.Ports = append(spec.Ports, vm.Forward{
				HostAddr:  forward.Host,
				GuestAddr: forward.Guest,
				Proto:     forward.Proto,
				FromGuest: forward.From == "guest",
			})
		}
	}
	files, err := loadFiles(document.WriteFiles)
	if err != nil {
		return nil, err
	}
	spec.Files = files
	return spec, nil
}

func loadConfig(document internalmanifest.Document) qemu.Config {
	config := qemu.Config{
		Machine:        document.Machine.Type,
		MachineOptions: document.QEMU.MachineOptions,
		CPU:            document.Machine.CPU,
		KVM:            document.Machine.KVM,
		Console:        qemu.Console(document.Kernel.Serial),
		Seccomp:        document.QEMU.Seccomp,
		HostName:       document.HostName,
		CIDRange:       qemu.CIDRange{Min: document.VSock.CIDRange.Min, Max: document.VSock.CIDRange.Max},
		Balloon:        document.Balloon != nil && document.Balloon.Enabled,
	}
	if len(document.QEMU.Exec) > 0 {
		config.Binary = document.QEMU.Exec[0]
		config.ExtraArgs = document.QEMU.Exec[1:]
	}
	for _, mount := range document.Mounts.VirtioFS() {
		if mount.VirtioFS.Bin != "" {
			config.VirtiofsdBinary = mount.VirtioFS.Bin
		}
		if len(mount.VirtioFS.Args) > 0 {
			config.VirtiofsdArgs = mount.VirtioFS.Args
		}
	}
	return config
}

func loadFiles(inputs []internalmanifest.WriteFileInput) ([]vm.File, error) {
	files := make([]vm.File, 0, len(inputs))
	for i, input := range inputs {
		mode, err := parseFileMode(input.Mode)
		if err != nil {
			return nil, fmt.Errorf("manifest.write_files[%d].mode: %w", i, err)
		}
		content, err := fileContent(input)
		if err != nil {
			return nil, fmt.Errorf("manifest.write_files[%d]: %w", i, err)
		}
		files = append(files, vm.File{GuestPath: input.GuestPath, Content: content, Mode: mode})
	}
	return files, nil
}

func fileContent(input internalmanifest.WriteFileInput) (io.Reader, error) {
	if input.Text != nil {
		return bytes.NewReader([]byte(*input.Text)), nil
	}
	if input.Path == nil || *input.Path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(*input.Path)
	if err != nil {
		return nil, fmt.Errorf("read source %q: %w", *input.Path, err)
	}
	return bytes.NewReader(data), nil
}

func parseFileMode(mode *string) (os.FileMode, error) {
	if mode == nil || *mode == "" {
		return 0, nil
	}
	parsed, err := strconv.ParseUint(*mode, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("%q is not an octal file mode", *mode)
	}
	return os.FileMode(parsed), nil
}

// checkSupported reports the machine-describing manifest features the neutral
// Spec cannot carry yet. Each one would otherwise change how the machine runs
// without appearing anywhere in the loaded Spec.
func checkSupported(document internalmanifest.Document) error {
	if len(document.Mounts.NineP()) > 0 {
		return unsupported("9p mounts", "use a virtiofs mount, or launch through the virtle CLI")
	}
	for _, mount := range document.Mounts.VirtioFS() {
		if socket := mount.VirtioFS.Socket; socket != "" && socket != mount.Tag+".sock" {
			return unsupported(
				fmt.Sprintf("a custom virtiofs socket path (%q on mount %q)", socket, mount.Tag),
				"the backend places share sockets itself; drop virtiofs.socket or launch through the virtle CLI")
		}
	}
	for _, file := range document.WriteFiles {
		if file.Chown != nil && *file.Chown != "" {
			return unsupported(
				fmt.Sprintf("write_files chown (%q on %q)", *file.Chown, file.GuestPath),
				"set ownership from the guest after boot")
		}
	}
	if document.Workspace.MountCWD {
		return unsupported("workspace.mount_cwd", "declare the directory as a virtiofs mount")
	}
	if len(document.QEMU.FwdTunnelExec) > 0 {
		return unsupported("qemu.fwd_tunnel_exec", "launch through the virtle CLI")
	}
	if document.Balloon != nil && document.Balloon.Controller != nil {
		return unsupported(
			"the balloon controller",
			"drive backend.MemoryResizer from your own policy loop, or launch through the virtle CLI")
	}
	return nil
}

func unsupported(feature string, alternative string) error {
	return fmt.Errorf("manifest uses %s, which the library API does not support yet: %s", feature, alternative)
}

// LoadFile reads a manifest from a host path. It is Load plus opening the
// file, and reports paths in errors.
func LoadFile(path string) (*vm.Spec, backend.Backend, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()

	spec, b, err := Load(file)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", filepath.Clean(path), err)
	}
	return spec, b, nil
}
