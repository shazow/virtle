package manifest_test

import (
	"io"
	"strings"
	"testing"

	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/manifest"
	"github.com/shazow/virtle/units"
)

const minimalManifest = `
[kernel]
path = "/tmp/vmlinuz"
initrd_path = "/tmp/initrd"
`

func TestLoadAppliesManifestDefaults(t *testing.T) {
	spec, b, err := manifest.Load(strings.NewReader(minimalManifest))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if b == nil {
		t.Fatal("Load() returned no backend")
	}
	if got := spec.Kernel.Path; got != "/tmp/vmlinuz" {
		t.Errorf("kernel path = %q", got)
	}
	if got := spec.Memory; got != 1024*units.Mebibyte {
		t.Errorf("memory = %s, want the manifest's 1024 MiB default", got)
	}
	if got := spec.Dir; got != ".virtle" {
		t.Errorf("dir = %q, want the manifest's default state dir", got)
	}
	if _, ok := b.(backend.Suspender); !ok {
		t.Error("loaded backend does not satisfy backend.Suspender")
	}
}

func TestLoadLowersMachineDescription(t *testing.T) {
	const document = `
host_name = "sandbox"
state_dir = "/run/sandbox"

[machine]
vcpu = 4
memory = 2048

[kernel]
path = "vmlinuz"
initrd_path = "initrd.img"
params = ["quiet", "loglevel=3"]

[[mounts]]
type = "virtiofs"
tag = "workspace"
source = "."
virtiofs.socket = "workspace.sock"

[[mounts]]
type = "image"
source = "root.img"
image.size = 4096
image.create = true

[[networks]]
id = "microvm1"
type = "user"
mac = "02:02:00:00:00:01"
[[networks.forward]]
proto = "tcp"
from = "host"
host = "127.0.0.1:2222"
guest = "10.0.2.15:22"

[[write_files]]
guest_path = "/etc/motd"
text = "welcome\n"
mode = "0644"
`
	spec, _, err := manifest.Load(strings.NewReader(document))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if got := spec.CPUs; got != 4 {
		t.Errorf("cpus = %d, want 4", got)
	}
	if got := spec.Memory; got != 2048*units.Mebibyte {
		t.Errorf("memory = %s, want 2 GiB", got)
	}
	if got := spec.Kernel.Cmdline; got != "quiet loglevel=3" {
		t.Errorf("cmdline = %q", got)
	}
	if got := spec.Dir; got != "/run/sandbox" {
		t.Errorf("dir = %q", got)
	}

	if len(spec.Shares) != 1 || spec.Shares[0].Tag != "workspace" || spec.Shares[0].HostPath != "." {
		t.Errorf("shares = %+v, want the workspace virtiofs mount", spec.Shares)
	}
	if len(spec.Disks) != 1 || spec.Disks[0].Path != "root.img" || spec.Disks[0].Size != 4096*units.Mebibyte {
		t.Errorf("disks = %+v, want the 4 GiB root image", spec.Disks)
	}
	if len(spec.Ports) != 1 || spec.Ports[0].HostAddr != "127.0.0.1:2222" || spec.Ports[0].FromGuest {
		t.Errorf("ports = %+v, want one host-origin forward", spec.Ports)
	}

	if len(spec.Files) != 1 {
		t.Fatalf("files = %+v, want one write_files entry", spec.Files)
	}
	if got := spec.Files[0].Mode.Perm(); got != 0o644 {
		t.Errorf("file mode = %o, want 0644", got)
	}
	content, err := io.ReadAll(spec.Files[0].Content)
	if err != nil {
		t.Fatalf("read file content: %v", err)
	}
	if string(content) != "welcome\n" {
		t.Errorf("file content = %q", content)
	}
}

func TestLoadRejectsFeaturesTheSpecCannotCarry(t *testing.T) {
	for _, tt := range []struct {
		name     string
		document string
		want     string
	}{
		{
			name: "9p mount",
			document: minimalManifest + `
[[mounts]]
type = "9p"
tag = "cache"
source = ".cache"
`,
			want: "9p",
		},
		{
			name: "custom virtiofs socket",
			document: minimalManifest + `
[[mounts]]
type = "virtiofs"
tag = "workspace"
source = "."
virtiofs.socket = "elsewhere.sock"
`,
			want: "socket",
		},
		{
			name: "write_files chown",
			document: minimalManifest + `
[[write_files]]
guest_path = "/etc/motd"
text = "hi"
chown = "root:root"
`,
			want: "chown",
		},
		{
			name: "balloon controller",
			document: minimalManifest + `
[balloon]
enabled = true
[balloon.controller]
min_actual = 256
max_actual = 512
`,
			want: "balloon controller",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := manifest.Load(strings.NewReader(tt.document))
			if err == nil {
				t.Fatalf("Load() error = nil, want an unsupported-feature error naming %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Load() error = %v, want it to name %q", err, tt.want)
			}
		})
	}
}

func TestLoadIgnoresSessionOrchestrationSections(t *testing.T) {
	// run, ssh, and notifications describe a CLI session rather than a
	// machine, so the library loads the manifest without them.
	document := minimalManifest + `
[[run]]
exec = ["echo", "hello"]

[ssh]
user = "agent"

[notifications]
exec = ["notify-send", "{{.State}}"]
states = ["ready"]
`
	if _, _, err := manifest.Load(strings.NewReader(document)); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestLoadReportsManifestErrors(t *testing.T) {
	if _, _, err := manifest.Load(strings.NewReader("[kernel]\n")); err == nil {
		t.Fatal("Load() error = nil, want a missing-kernel error")
	}
	if _, _, err := manifest.Load(strings.NewReader("not = valid = toml")); err == nil {
		t.Fatal("Load() error = nil, want a decode error")
	}
	if _, _, err := manifest.Load(nil); err == nil {
		t.Fatal("Load(nil) error = nil, want a missing-reader error")
	}
}
