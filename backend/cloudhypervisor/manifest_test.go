package cloudhypervisor

import (
	"strings"
	"testing"

	imanifest "github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/units"
	"github.com/shazow/virtle/vm"
)

func TestManifestOverlay(t *testing.T) {
	doc, err := imanifest.DecodeDocumentBytes([]byte(`backend="cloud-hypervisor"
working_dir="/base"
state_dir="state"
[machine]
vcpu=2
memory=256
[kernel]
path="kernel"
serial="print"
[cloud-hypervisor]
binary="./bin/cloud-hypervisor"
startup_timeout="3s"
[[mounts]]
type="virtiofs"
tag="src"
source="original-src"
virtiofs.socket="custom.sock"
virtiofs.bin="/opt/virtiofsd"
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
	spec := &vm.Spec{Dir: "/override", CPUs: 4, Memory: 512 * units.Mebibyte,
		Shares: []vm.Share{{Tag: "src", HostPath: "replacement-src"}},
		Disks:  []vm.Disk{{Path: "replacement", ReadOnly: true, GuestPath: "/"}}}
	mf, err := b.resolveSpec(spec, "")
	if err != nil {
		t.Fatal(err)
	}
	ch := mf.CloudHypervisor
	if ch.CPUs != 4 || ch.MemoryMiB != 512 || ch.Kernel.Path != "/override/kernel" || ch.Binary != "/override/bin/cloud-hypervisor" || ch.Console != imanifest.KernelSerialPrint || ch.StartupTimeout.String() != "3s" {
		t.Fatalf("configuration %+v", ch)
	}
	if !strings.HasSuffix(ch.Kernel.Cmdline, " root=/dev/vda ro") || !strings.HasPrefix(ch.Kernel.Cmdline, "console=") {
		t.Fatalf("cmdline %q", ch.Kernel.Cmdline)
	}
	if mf.ResolvedPersistenceStateDir() != "/override/state" || len(ch.Disks) != 1 || ch.Disks[0].Path != "/override/replacement" || !ch.Disks[0].ReadOnly {
		t.Fatalf("manifest %+v, disks %+v", mf, ch.Disks)
	}
	// Spec entries overlay the manifest's mounts by position: settings the
	// Spec cannot express (image.label, the share's socket and daemon)
	// survive, while the Spec's zero Size withdraws the manifest's create
	// request, as on QEMU.
	if ch.Disks[0].Label != "data" || ch.Disks[0].Create {
		t.Fatalf("disk overlay lost manifest settings: %+v", ch.Disks[0])
	}
	if len(ch.Shares) != 1 || ch.Shares[0].Source != "/override/replacement-src" || ch.Shares[0].Socket != "/override/state/custom.sock" {
		t.Fatalf("share overlay: %+v", ch.Shares)
	}
	if runs, err := mf.ResolvedRuns(0); err != nil || len(runs) != 1 || runs[0].Exec[0] != "/override/opt/virtiofsd" && runs[0].Exec[0] != "/opt/virtiofsd" {
		t.Fatalf("share daemon: %+v %v", runs, err)
	}
	spec.Disks, spec.Shares = nil, nil
	mf, err = b.resolveSpec(spec, "")
	if err != nil || len(mf.CloudHypervisor.Disks) != 0 || len(mf.CloudHypervisor.Shares) != 0 {
		t.Fatalf("removing mounts: %v %v", mf, err)
	}
	if doc.Mounts.Image()[0].SourcePath != "original" || doc.Mounts.VirtioFS()[0].SourcePath != "original-src" || !strings.Contains(doc.CloudHypervisor.Binary, "./bin/") {
		t.Fatal("source document was mutated")
	}
}
