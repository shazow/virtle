package manifest

import (
	"io"
	"strings"
	"testing"

	"github.com/shazow/virtle/units"
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
