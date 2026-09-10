package manifest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shazow/virtle/backend/qemu"
	"github.com/shazow/virtle/units"
	"github.com/shazow/virtle/vmnet"
	"github.com/shazow/virtle/vmnet/egress"
	"github.com/shazow/virtle/vmnet/userspace"
)

const testManifest = `
[machine]
vcpu = 2
memory = 512

[kernel]
path = "vmlinuz"
initrd_path = "initrd.img"
params = ["quiet"]

[[mounts]]
type = "virtiofs"
tag = "src"
source = "."
target = "/workspace"
read_only = true
virtiofs.socket = "src.sock"

[[mounts]]
type = "image"
source = "data.img"
[mounts.image]
size = 256
format = "raw"
create = true

[[networks]]
id = "net1"
type = "user"
mac = "02:02:00:00:00:01"

[[networks.forward]]
proto = "tcp"
from = "host"
host = "127.0.0.1:8080"
guest = "10.0.2.15:80"

[[write_files]]
guest_path = "/etc/motd"
text = "hello"
mode = "0644"
`

func TestLoad(t *testing.T) {
	spec, b, err := Load(strings.NewReader(testManifest))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if b == nil {
		t.Fatal("Load returned nil backend")
	}

	if got, want := spec.CPUs, 2; got != want {
		t.Errorf("CPUs = %d, want %d", got, want)
	}
	if got, want := spec.Memory, 512*units.Mebibyte; got != want {
		t.Errorf("Memory = %s, want %s", got, want)
	}
	if spec.Kernel.Path != "vmlinuz" || spec.Kernel.Initrd != "initrd.img" || spec.Kernel.Cmdline != "quiet" {
		t.Errorf("Kernel = %+v", spec.Kernel)
	}
	if len(spec.Shares) != 1 || spec.Shares[0].Tag != "src" || spec.Shares[0].GuestPath != "/workspace" || !spec.Shares[0].ReadOnly {
		t.Errorf("Shares = %+v", spec.Shares)
	}
	if len(spec.Disks) != 1 || spec.Disks[0].Path != "data.img" || spec.Disks[0].Size != 256*units.Mebibyte || spec.Disks[0].Format != "raw" {
		t.Errorf("Disks = %+v", spec.Disks)
	}
	if len(spec.Ports) != 1 || spec.Ports[0].HostAddr != "127.0.0.1:8080" || spec.Ports[0].GuestAddr != "10.0.2.15:80" {
		t.Errorf("Ports = %+v", spec.Ports)
	}
	if len(spec.Files) != 1 || spec.Files[0].GuestPath != "/etc/motd" || spec.Files[0].Mode != 0o644 {
		t.Fatalf("Files = %+v", spec.Files)
	}
	content, err := io.ReadAll(spec.Files[0].Content)
	if err != nil || string(content) != "hello" {
		t.Errorf("file content = %q (%v), want %q", content, err, "hello")
	}
	if spec.Dir == "" {
		t.Error("Dir is empty, want the resolved working directory")
	}
}

// TestLoadNamesTheRootDisk guards the Spec lowering of target = "/": the
// backends trust the Spec alone at Start (it overlays the document's
// mounts), so a root device dropped here would be lost on every launch.
func TestLoadNamesTheRootDisk(t *testing.T) {
	spec, _, err := Load(strings.NewReader("[kernel]\npath = \"vmlinuz\"\n[[mounts]]\ntype = \"image\"\nsource = \"root.img\"\ntarget = \"/\"\nread_only = true\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(spec.Disks) != 1 || spec.Disks[0].GuestPath != "/" || !spec.Disks[0].ReadOnly {
		t.Fatalf("Disks = %+v, want the root image with GuestPath \"/\"", spec.Disks)
	}
}

func TestLoadRejectsInvalidManifest(t *testing.T) {
	// A boot from disks without an initrd needs a root device.
	if _, _, err := Load(strings.NewReader("[kernel]\npath = \"vmlinuz\"\n[[mounts]]\ntype = \"image\"\nsource = \"data.img\"\n")); err == nil {
		t.Fatal("expected invalid manifest to fail at Load")
	}
}

func TestLoadJSON(t *testing.T) {
	spec, _, err := Load(strings.NewReader(`{"kernel": {"path": "vmlinuz", "initrd_path": "initrd.img"}}`))
	if err != nil {
		t.Fatalf("Load JSON: %v", err)
	}
	if spec.Kernel.Path != "vmlinuz" {
		t.Errorf("Kernel = %+v", spec.Kernel)
	}
}

const virtleNetworkManifest = `
[kernel]
path = "vmlinuz"

[[networks]]
type = "virtle"
forward = [{ host = "127.0.0.1:2222", guest = ":22" }]
`

// idleLink is a link whose peer never speaks, for attaching without a guest.
func idleLink(t *testing.T, mtu int) vmnet.Link {
	t.Helper()
	hostEnd, guestEnd := net.Pipe()
	t.Cleanup(func() { _ = guestEnd.Close() })
	return vmnet.QEMUStream(hostEnd, mtu)
}

func TestLoadBuildsVirtleNetwork(t *testing.T) {
	spec, b, err := Load(strings.NewReader(virtleNetworkManifest))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	qb, ok := b.(*qemu.Backend)
	if !ok {
		t.Fatalf("backend = %T, want *qemu.Backend", b)
	}
	network, ok := qb.Network.(*userspace.Network)
	if !ok {
		t.Fatalf("Network = %T, want a userspace network", qb.Network)
	}
	if qb.Link != nil {
		t.Errorf("Link = %v, want nil so the manifest's type decides", qb.Link)
	}
	if len(spec.Ports) != 1 || spec.Ports[0].GuestAddr != ":22" {
		t.Errorf("Ports = %+v, want the forward", spec.Ports)
	}

	// The network logs through the backend's Logger, wherever it points by
	// the time something happens, the way the CLI wires loggers after Load.
	var logs bytes.Buffer
	qb.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	port, err := network.Attach(context.Background(), idleLink(t, network.MTU()), vmnet.AttachOptions{Name: "vm1"})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	_ = port.Close()
	if got := logs.String(); !strings.Contains(got, "network port attached") || !strings.Contains(got, "package=vmnet") {
		t.Errorf("network logs did not reach the backend's logger: %q", got)
	}

	closer, ok := b.(io.Closer)
	if !ok {
		t.Fatal("a backend that owns a network does not implement io.Closer")
	}
	if err := closer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := network.Attach(context.Background(), idleLink(t, network.MTU()), vmnet.AttachOptions{}); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Attach after Close = %v, want net.ErrClosed", err)
	}
	if err := closer.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestLoadOwnsNoNetworkForOtherTypes(t *testing.T) {
	for name, doc := range map[string]string{
		"user": testManifest,
		"tap":  "[kernel]\npath = \"vmlinuz\"\n\n[[networks]]\ntype = \"tap\"\ntap = \"tap0\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, b, err := Load(strings.NewReader(doc))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			qb := b.(*qemu.Backend)
			if qb.Network != nil || qb.Link != nil {
				t.Fatalf("Network = %v, Link = %v; want neither", qb.Network, qb.Link)
			}
			if err := qb.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
		})
	}
}

func TestLoadBuildsEgressPolicy(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GH_TOKEN", "ghp_secret")
	doc := `working_dir = "` + dir + `"

[kernel]
path = "vmlinuz"

[[networks]]
type = "virtle"

[egress]
reach = "rules"
[[egress.allow]]
host = "api.github.com"
ports = [443]
inspect = true

[[egress.deny]]
host = "uploads.github.com"

[[egress.secrets]]
name = "GITHUB_TOKEN"
from = "{{.Env.GH_TOKEN}}"
hosts = ["api.github.com"]
`
	spec, b, err := Load(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	qb := b.(*qemu.Backend)
	defer qb.Close()
	network, ok := qb.Network.(*userspace.Network)
	if !ok || network.DNS() != userspace.DNSFakeIP {
		t.Fatalf("network = %T; a policy needs the fake-IP DNS mode", qb.Network)
	}
	if spec.Egress == nil || len(spec.Egress.Allow) != 1 || spec.Egress.Allow[0].Host != "api.github.com" || len(spec.Egress.Deny) != 1 || len(spec.Egress.Secrets) != 1 || spec.Egress.Secrets[0] != "GITHUB_TOKEN" {
		t.Fatalf("Spec.Egress = %+v", spec.Egress)
	}
	files := map[string]string{}
	for _, f := range spec.Files {
		content, _ := io.ReadAll(f.Content)
		files[f.GuestPath] = string(content)
	}
	if !strings.HasPrefix(files[egress.GuestCAPath], "-----BEGIN CERTIFICATE-----") {
		t.Errorf("CA file = %q", files[egress.GuestCAPath])
	}
	if !strings.Contains(files[GuestSecretsPath], "export GITHUB_TOKEN=virtle_GITHUB_TOKEN_") || strings.Contains(files[GuestSecretsPath], "ghp_secret") {
		t.Errorf("secrets file = %q", files[GuestSecretsPath])
	}
	if _, err := os.Stat(filepath.Join(dir, ".virtle", "egress-ca", "ca.pem")); err != nil {
		t.Errorf("CA was not created under the state directory: %v", err)
	}

	// Without inspection there is no CA and nothing to give the guest.
	plain := strings.Replace(strings.Replace(doc, "inspect = true\n", "", 1), "[[egress.secrets]]\nname = \"GITHUB_TOKEN\"\nfrom = \"{{.Env.GH_TOKEN}}\"\nhosts = [\"api.github.com\"]\n", "", 1)
	spec, b, err = Load(strings.NewReader(plain))
	if err != nil {
		t.Fatalf("Load without inspection: %v", err)
	}
	defer b.(io.Closer).Close()
	if len(spec.Files) != 0 || spec.Egress == nil || len(spec.Egress.Secrets) != 0 {
		t.Fatalf("Files = %+v, Egress = %+v", spec.Files, spec.Egress)
	}
}

func TestLoadVirtleNetworkReachesTheInternetByDefault(t *testing.T) {
	spec, b, err := Load(strings.NewReader(virtleNetworkManifest))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	qb := b.(*qemu.Backend)
	defer qb.Close()
	// The policy is there (the network resolves names for it), the guest
	// has nothing of its own to add to it, and gets no files.
	if network := qb.Network.(*userspace.Network); network.DNS() != userspace.DNSFakeIP {
		t.Fatalf("DNS mode = %s; a virtle network carries a policy by default", network.DNS())
	}
	if spec.Egress != nil || len(spec.Files) != 0 {
		t.Fatalf("Egress = %+v, Files = %+v; want the network's default policy and no files", spec.Egress, spec.Files)
	}

	// Deny entries under the default reach: the guest's view says
	// everything but them, and the policy narrows everything to the internet.
	spec, b, err = Load(strings.NewReader(virtleNetworkManifest + "\n[egress]\n[[egress.deny]]\nhost = \"tracker.test\"\n"))
	if err != nil {
		t.Fatalf("Load with a deny entry: %v", err)
	}
	defer b.(io.Closer).Close()
	if spec.Egress == nil || len(spec.Egress.Deny) != 1 || len(spec.Egress.Allow) != 3 || spec.Egress.Allow[0].Host != "*" || spec.Egress.Allow[1].Host != "0.0.0.0/0" {
		t.Fatalf("Egress = %+v", spec.Egress)
	}
	if _, _, err := Load(strings.NewReader(virtleNetworkManifest + "\n[egress]\nreach = \"lan\"\n")); err == nil || !strings.Contains(err.Error(), "reach") {
		t.Fatalf("unknown reach loaded: %v", err)
	}
}
