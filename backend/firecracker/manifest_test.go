package firecracker

import (
	"strings"
	"testing"

	imanifest "github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/units"
	"github.com/shazow/virtle/vm"
)

func TestManifestOverlay(t *testing.T) {
	doc, err := imanifest.DecodeDocumentBytes([]byte(`backend="firecracker"
working_dir="/base"
state_dir="state"
[machine]
vcpu=2
memory=256
[kernel]
path="kernel"
serial="print"
[firecracker]
binary="./bin/firecracker"
startup_timeout="3s"
[[mounts]]
type="image"
source="original"
target="/"
read_only=true
image.label="data"
image.create=true
image.size=256
`), "")
	if err != nil {
		t.Fatal(err)
	}
	b := NewBackendFromDocument(doc, Backend{}).(*Backend)
	spec := &vm.Spec{Dir: "/override", CPUs: 4, Memory: 512 * units.Mebibyte, Disks: []vm.Disk{{Path: "replacement", ReadOnly: true, GuestPath: "/"}}}
	mf, err := b.resolveSpec(spec, "")
	if err != nil {
		t.Fatal(err)
	}
	fc := mf.Firecracker
	if fc.CPUs != 4 || fc.MemoryMiB != 512 || fc.Kernel.Path != "/override/kernel" || fc.Binary != "/override/bin/firecracker" || fc.Console != imanifest.KernelSerialPrint || fc.StartupTimeout.String() != "3s" {
		t.Fatalf("configuration %+v", fc)
	}
	if fc.Kernel.Cmdline != "console=ttyS0 reboot=k panic=-1 root=/dev/vda ro" {
		t.Fatalf("cmdline %q", fc.Kernel.Cmdline)
	}
	if mf.ResolvedPersistenceStateDir() != "/override/state" || len(fc.Disks) != 1 || fc.Disks[0].Path != "/override/replacement" || !fc.Disks[0].ReadOnly {
		t.Fatalf("manifest %+v, disks %+v", mf, fc.Disks)
	}
	// The Spec disk overlays the manifest's mount by position: settings the
	// Spec cannot express (image.label) survive, while the Spec's zero Size
	// withdraws the manifest's create request, as on QEMU.
	if fc.Disks[0].Label != "data" || fc.Disks[0].Create {
		t.Fatalf("disk overlay lost manifest settings: %+v", fc.Disks[0])
	}
	spec.Disks = nil
	mf, err = b.resolveSpec(spec, "")
	if err != nil || len(mf.Firecracker.Disks) != 0 {
		t.Fatalf("removing disk: %v %v", mf, err)
	}
	if doc.Mounts.Image()[0].SourcePath != "original" || !strings.Contains(doc.Firecracker.Binary, "./bin/") {
		t.Fatal("source document was mutated")
	}
}
