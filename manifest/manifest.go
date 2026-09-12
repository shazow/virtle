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
	"github.com/shazow/virtle/backend/cloudhypervisor"
	"github.com/shazow/virtle/backend/firecracker"
	"github.com/shazow/virtle/backend/qemu"
	imanifest "github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/vm"
	"github.com/shazow/virtle/vmnet/egress"
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
// The network's policy (vmnet/egress) comes from the [egress] section, and
// without one reaches the internet and nothing on the host or its
// networks; the section's entries also become the Spec's Egress, and the
// guest gets the policy's CA certificate and its secret tokens as files
// (egress.GuestCAPath, GuestSecretsPath).
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
	mf, err := doc.Manifest()
	if err != nil {
		return nil, nil, err
	}
	withDefaults := imanifest.DocumentWithDefaults(doc)
	spec, err := specFromDocument(withDefaults)
	if err != nil {
		return nil, nil, err
	}
	switch doc.Backend {
	case imanifest.BackendFirecracker:
		return spec, firecracker.NewBackendFromDocument(doc, firecracker.Backend{}), nil
	case imanifest.BackendCloudHypervisor:
		return spec, cloudhypervisor.NewBackendFromDocument(doc, cloudhypervisor.Backend{}), nil
	}
	var cfg qemu.Backend
	// The backend is built after the network, so the network's logger looks
	// the backend up when it logs rather than capturing it here.
	var loaded *qemu.Backend
	if slices.ContainsFunc(mf.QEMU.Devices.Network, func(d imanifest.QEMUNetDevice) bool { return d.Managed }) {
		logger := slog.New(delegatingHandler{get: func() slog.Handler {
			if loaded != nil && loaded.Logger != nil {
				return loaded.Logger.Handler()
			}
			return slog.DiscardHandler
		}})
		netCfg := userspace.Config{Logger: logger.With("package", "vmnet")}
		if mf.Egress != nil {
			policy, err := egressPolicy(mf.Egress, logger.With("package", "egress"))
			if err != nil {
				return nil, nil, err
			}
			netCfg.Egress = policy
			spec.Egress = specEgress(mf.Egress)
			spec.Files = append(spec.Files, policy.GuestFiles()...)
			spec.Files = append(spec.Files, guestSecretsFile(policy, spec.Egress)...)
		}
		network, err := userspace.New(netCfg)
		if err != nil {
			return nil, nil, fmt.Errorf("create virtle network: %w", err)
		}
		cfg.Network = network
	}
	loaded = qemu.NewBackendFromDocument(doc, cfg).(*qemu.Backend)
	return spec, loaded, nil
}

// GuestSecretsPath is where a guest finds the tokens of its secrets, as
// shell export lines.
const GuestSecretsPath = "/etc/virtle/secrets.env"

// egressPolicy builds the network's policy from the manifest's [egress]
// section, creating the CA when an entry inspects.
func egressPolicy(e *imanifest.Egress, logger *slog.Logger) (*egress.Policy, error) {
	policy := &egress.Policy{Logger: logger, Reach: egress.Reach(e.Reach)}
	for _, r := range e.Allow {
		policy.Rules = append(policy.Rules, egress.Rule{Hosts: []string{r.Host}, Ports: r.Ports, Inspect: r.Inspect})
	}
	for _, s := range e.Secrets {
		// A secret is a named injection: the guest gets a generated token
		// and the value is read when a request carries it.
		value := s.ValueFunc()
		inj := egress.Injection{
			Name:  s.Name,
			Value: func(context.Context, egress.Request) (string, error) { return value() },
			Hosts: s.Hosts, Methods: s.Methods, Paths: s.Paths,
		}
		for _, in := range s.In {
			inj.In = append(inj.In, egress.Placement(in))
		}
		policy.Injections = append(policy.Injections, inj)
	}
	if e.Inspects {
		ca, err := egress.LoadOrCreateCA(e.CADir)
		if err != nil {
			return nil, err
		}
		policy.CA = ca
	}
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	return policy, nil
}

// specEgress is the guest's view of the [egress] section: the same allow
// and deny entries and the names of every secret it may hold, with a reach
// beyond the entries spelled out as patterns for everything, which the
// network's policy narrows to the internet or not. Nil when the section
// adds nothing to the network's policy, which is then the guest's.
func specEgress(e *imanifest.Egress) *vm.Egress {
	if len(e.Allow) == 0 && len(e.Deny) == 0 && len(e.Secrets) == 0 {
		return nil
	}
	reaches := func(rules []imanifest.EgressRule) []vm.Reach {
		out := make([]vm.Reach, 0, len(rules))
		for _, r := range rules {
			out = append(out, vm.Reach{Host: r.Host, Ports: r.Ports})
		}
		return out
	}
	allow := reaches(e.Allow)
	if egress.Reach(e.Reach) != egress.ReachRules {
		allow = append(allow, vm.Reach{Host: "*"}, vm.Reach{Host: "0.0.0.0/0"}, vm.Reach{Host: "::/0"})
	}
	names := make([]string, 0, len(e.Secrets))
	for _, s := range e.Secrets {
		names = append(names, s.Name)
	}
	return &vm.Egress{Allow: allow, Deny: reaches(e.Deny), Secrets: names}
}

// guestSecretsFile places the guest's tokens at GuestSecretsPath; a token
// is worthless outside the guest, so the file is readable by any user in
// it.
func guestSecretsFile(policy *egress.Policy, e *vm.Egress) []vm.File {
	env := policy.GuestEnv(e)
	if len(env) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("# Tokens virtle replaces with the real secrets in inspected requests.\n")
	for _, kv := range env {
		b.WriteString("export " + kv + "\n")
	}
	return []vm.File{{GuestPath: GuestSecretsPath, Content: strings.NewReader(b.String()), Mode: 0o644}}
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
