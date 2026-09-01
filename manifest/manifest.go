// Package manifest loads virtle manifests (TOML or JSON) and lowers them
// to a neutral vm.Spec plus the backend they configure. It is one Spec
// source among many — Specs constructed in Go are equally valid.
//
// CLI-only manifest concerns — host [run] helper commands, [ssh] session
// handling, [notifications] — configure session orchestration, not the
// VM, and stay with the virtle CLI.
package manifest

import (
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/backend/qemu"
	imanifest "github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/units"
	"github.com/shazow/virtle/vm"
)

// Load reads a virtle manifest and lowers it to a neutral Spec plus the
// backend it configures. The returned backend preserves manifest detail
// beyond what Spec models (balloon controller, hotplug table, write-file
// ownership, ...); the Spec is the neutral view, and edits to it are
// overlaid when the backend starts. Shares, disks, host port forwards, and
// inline files replace their corresponding loaded entries by slice position;
// removing an entry removes it from launch, and extra entries append. Manifest
// entries with no Spec representation remain backend-owned. Normal manifest
// defaulting and validation still apply to the combined result.
//
// A relative working_dir (including the "." default) resolves against the
// process working directory.
func Load(r io.Reader) (*vm.Spec, backend.Backend, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, nil, fmt.Errorf("read manifest: %w", err)
	}
	doc, err := imanifest.DecodeDocumentBytes(data, "")
	if err != nil {
		return nil, nil, err
	}

	workingDir := doc.WorkingDir
	if workingDir == "" {
		workingDir = "."
	}
	if !filepath.IsAbs(workingDir) {
		resolved, err := filepath.Abs(workingDir)
		if err != nil {
			return nil, nil, fmt.Errorf("resolve manifest working directory %q: %w", workingDir, err)
		}
		workingDir = resolved
	}
	doc.WorkingDir = workingDir

	// Resolve early so invalid manifests fail at Load, not at Start.
	if _, err := doc.Manifest(); err != nil {
		return nil, nil, err
	}

	spec, err := specFromDocument(imanifest.DocumentWithDefaults(doc))
	if err != nil {
		return nil, nil, err
	}
	return spec, qemu.NewBackendFromDocument(doc), nil
}

// specFromDocument extracts the neutral Spec view from a defaults-merged
// manifest document.
func specFromDocument(doc imanifest.Document) (*vm.Spec, error) {
	spec := &vm.Spec{
		CPUs:   doc.Machine.VCPU,
		Memory: units.Bytes(doc.Machine.Memory.Bytes()),
		Kernel: vm.Kernel{
			Path:    doc.Kernel.Path,
			Initrd:  doc.Kernel.InitrdPath,
			Cmdline: strings.Join(doc.Kernel.Params, " "),
		},
		Dir: doc.WorkingDir,
	}

	for _, mount := range doc.Mounts.VirtioFS() {
		spec.Shares = append(spec.Shares, vm.Share{
			Tag:       mount.Tag,
			HostPath:  mount.SourcePath,
			GuestPath: mount.Target,
			ReadOnly:  mount.ReadOnly,
		})
	}
	for _, mount := range doc.Mounts.Image() {
		spec.Disks = append(spec.Disks, vm.Disk{
			Path:   mount.SourcePath,
			Format: mount.Image.Format,
			Size:   units.Bytes(mount.Image.Size.Bytes()),
		})
	}
	for _, network := range doc.Networks {
		for _, forward := range network.Forward {
			if forward.From != "" && forward.From != "host" {
				continue // guest-to-host forwards have no Spec equivalent yet
			}
			spec.Ports = append(spec.Ports, vm.Forward{
				HostAddr:  forward.Host,
				GuestAddr: forward.Guest,
				Proto:     vm.Proto(forward.Proto),
			})
		}
	}
	for _, file := range doc.WriteFiles {
		if file.Text == nil {
			continue // host-sourced files stay a backend concern
		}
		f := vm.File{
			GuestPath: file.GuestPath,
			Content:   strings.NewReader(*file.Text),
		}
		if file.Mode != nil {
			mode, err := strconv.ParseUint(*file.Mode, 8, 32)
			if err != nil {
				return nil, fmt.Errorf("write file %q: invalid mode %q: %w", file.GuestPath, *file.Mode, err)
			}
			f.Mode = fs.FileMode(mode)
		}
		spec.Files = append(spec.Files, f)
	}
	return spec, nil
}
