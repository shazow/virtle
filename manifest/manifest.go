// Package manifest loads virtle manifests (TOML or JSON) and lowers them
// to a neutral vm.Spec plus the backend they configure. It is one Spec
// source among many — Specs constructed in Go are equally valid.
//
// Manifest sections with no vm.Spec representation — host [run] helper
// commands, [notifications] hooks, [ssh] settings — stay attached to the
// returned backend, which rejects the ones it cannot honor at load time.
// QEMU starts the helpers and runs the hooks itself; the interactive SSH
// session is driven by the virtle CLI's foreground loop.
package manifest

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/backend/firecracker"
	"github.com/shazow/virtle/backend/qemu"
	imanifest "github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/vm"
	"github.com/shazow/virtle/vmnet/userspace"
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
//
// A [[networks]] entry of type virtle makes the loader build a network
// virtle runs in userspace (vmnet/userspace) and hand it to the backend,
// which owns it: it logs through the backend's Logger, and closing the
// backend (it implements io.Closer) releases it once its machines are done.
func Load(r io.Reader) (*vm.Spec, backend.Backend, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, nil, fmt.Errorf("read manifest: %w", err)
	}
	doc, err := imanifest.DecodeDocumentBytes(data, "")
	if err != nil {
		return nil, nil, err
	}
	if err := doc.ResolveWorkingDir(); err != nil {
		return nil, nil, err
	}
	return LoadDocument(doc)
}

// LoadDocument lowers an already-decoded manifest document exactly as Load
// does, for callers that also need the document itself (the virtle CLI
// decodes once and reuses it). The document type is internal, so this is
// not callable (and not supported) outside the module.
func LoadDocument(doc imanifest.Document) (*vm.Spec, backend.Backend, error) {
	// Resolve early so invalid manifests fail at load, not at Start.
	if _, err := doc.Manifest(); err != nil {
		return nil, nil, err
	}
	withDefaults := imanifest.DocumentWithDefaults(doc)
	spec, err := specFromDocument(withDefaults)
	if err != nil {
		return nil, nil, err
	}
	if doc.Backend == imanifest.BackendFirecracker {
		return spec, firecracker.NewBackendFromDocument(doc, firecracker.Backend{}), nil
	}
	var cfg qemu.Backend
	// The backend is built after the network, so the network's logger looks
	// the backend up when it logs rather than capturing it here.
	var loaded *qemu.Backend
	if declaresVirtleNetwork(withDefaults.Networks) {
		logger := slog.New(delegatingHandler{get: func() slog.Handler {
			if loaded != nil && loaded.Logger != nil {
				return loaded.Logger.Handler()
			}
			return slog.DiscardHandler
		}}).With("package", "vmnet")
		network, err := userspace.New(userspace.Config{Logger: logger})
		if err != nil {
			return nil, nil, fmt.Errorf("create virtle network: %w", err)
		}
		cfg.Network = network
	}
	loaded = qemu.NewBackendFromDocument(doc, cfg).(*qemu.Backend)
	return spec, loaded, nil
}

// declaresVirtleNetwork reports whether a NIC attaches to a network virtle
// runs, which the loader must build.
func declaresVirtleNetwork(networks []imanifest.NetworkInput) bool {
	for _, network := range networks {
		if network.Type == imanifest.NetworkTypeVirtle {
			return true
		}
	}
	return false
}

// delegatingHandler resolves the handler it forwards to on every record,
// so a network built at load logs through whatever Logger the backend's
// owner sets afterwards, as the CLI does.
type delegatingHandler struct {
	get  func() slog.Handler
	wrap []func(slog.Handler) slog.Handler
}

func (h delegatingHandler) resolve() slog.Handler {
	handler := h.get()
	for _, wrap := range h.wrap {
		handler = wrap(handler)
	}
	return handler
}

func (h delegatingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.resolve().Enabled(ctx, level)
}

func (h delegatingHandler) Handle(ctx context.Context, record slog.Record) error {
	return h.resolve().Handle(ctx, record)
}

func (h delegatingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return h.with(func(handler slog.Handler) slog.Handler { return handler.WithAttrs(attrs) })
}

func (h delegatingHandler) WithGroup(name string) slog.Handler {
	return h.with(func(handler slog.Handler) slog.Handler { return handler.WithGroup(name) })
}

func (h delegatingHandler) with(wrap func(slog.Handler) slog.Handler) slog.Handler {
	return delegatingHandler{get: h.get, wrap: append(slices.Clone(h.wrap), wrap)}
}

// specFromDocument extracts the neutral Spec view from a defaults-merged
// manifest document.
func specFromDocument(doc imanifest.Document) (*vm.Spec, error) {
	spec := &vm.Spec{
		CPUs:   doc.Machine.VCPU,
		Memory: doc.Machine.Memory.Bytes(),
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
			ReadOnly:  mount.ReadOnly,
			Path:      mount.SourcePath,
			Format:    mount.Image.Format,
			Size:      mount.Image.Size.Bytes(),
			GuestPath: mount.Target, // "/" names the root device
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
