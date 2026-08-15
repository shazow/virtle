package qemu

import (
	"strings"
	"testing"

	imanifest "github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/units"
	"github.com/shazow/virtle/vm"
)

func testSpec() *vm.Spec {
	return &vm.Spec{
		CPUs:   2,
		Memory: 512 * units.Mebibyte,
		Kernel: vm.Kernel{Path: "vmlinuz", Initrd: "initrd.img", Cmdline: "quiet loglevel=3"},
		Shares: []vm.Share{{Tag: "src", HostPath: ".", GuestPath: "/workspace", ReadOnly: true}},
		Disks:  []vm.Disk{{Path: "data.img", Format: "raw", Size: 256 * units.Mebibyte}},
		Ports:  []vm.Forward{{HostAddr: "127.0.0.1:8080", GuestAddr: "10.0.2.15:80"}},
		Files:  []vm.File{{GuestPath: "/etc/motd", Content: strings.NewReader("hi"), Mode: 0o644}},
		Dir:    ".",
	}
}

func TestSpecDocument(t *testing.T) {
	doc, err := specDocument(testSpec(), Config{HostName: "testvm"}, nil)
	if err != nil {
		t.Fatalf("specDocument: %v", err)
	}

	if got, want := doc.HostName, "testvm"; got != want {
		t.Errorf("HostName = %q, want %q", got, want)
	}
	if got, want := doc.Machine.VCPU, 2; got != want {
		t.Errorf("VCPU = %d, want %d", got, want)
	}
	if got, want := int(doc.Machine.Memory), 512; got != want {
		t.Errorf("Memory = %d MiB, want %d", got, want)
	}
	if got, want := doc.Kernel.Path, "vmlinuz"; got != want {
		t.Errorf("Kernel.Path = %q, want %q", got, want)
	}
	if got, want := strings.Join(doc.Kernel.Params, " "), "quiet loglevel=3"; got != want {
		t.Errorf("Kernel.Params = %q, want %q", got, want)
	}

	shares := doc.Mounts.VirtioFS()
	if len(shares) != 1 || shares[0].Tag != "src" || !shares[0].ReadOnly || shares[0].Target != "/workspace" {
		t.Errorf("virtiofs mounts = %+v, want one src share", shares)
	}
	disks := doc.Mounts.Image()
	if len(disks) != 1 || disks[0].SourcePath != "data.img" || int(disks[0].Image.Size) != 256 || !disks[0].Image.AutoCreate {
		t.Errorf("image mounts = %+v, want one auto-created data.img", disks)
	}
	if len(doc.Networks) == 0 || len(doc.Networks[0].Forward) != 1 {
		t.Fatalf("networks = %+v, want one forward on the default network", doc.Networks)
	}
	fwd := doc.Networks[0].Forward[0]
	if fwd.Proto != "tcp" || fwd.From != "host" || fwd.Host != "127.0.0.1:8080" || fwd.Guest != "10.0.2.15:80" {
		t.Errorf("forward = %+v", fwd)
	}
	if len(doc.WriteFiles) != 1 || doc.WriteFiles[0].GuestPath != "/etc/motd" ||
		doc.WriteFiles[0].Text == nil || *doc.WriteFiles[0].Text != "hi" ||
		doc.WriteFiles[0].Mode == nil || *doc.WriteFiles[0].Mode != "644" {
		t.Errorf("write files = %+v", doc.WriteFiles)
	}
}

func TestSpecDocumentQGASocketOverride(t *testing.T) {
	doc, err := specDocument(testSpec(), Config{RemoteControl: QGA{SocketPath: "custom-qga.sock"}}, nil)
	if err != nil {
		t.Fatalf("specDocument: %v", err)
	}
	if got, want := doc.QEMU.GuestAgentSocket, "custom-qga.sock"; got != want {
		t.Errorf("GuestAgentSocket = %q, want %q", got, want)
	}
}

func TestSpecDocumentHotplugPorts(t *testing.T) {
	doc, err := specDocument(testSpec(), Config{HotplugPorts: 3}, nil)
	if err != nil {
		t.Fatalf("specDocument: %v", err)
	}
	if got, want := doc.Hotplug.Ports, 3; got != want {
		t.Errorf("Hotplug.Ports = %d, want %d", got, want)
	}
	mf, err := doc.Manifest()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got, want := mf.QEMU.Hotplug.PCIEPorts, 3; got != want {
		t.Errorf("PCIEPorts = %d, want %d", got, want)
	}
}

func TestSpecDocumentResolves(t *testing.T) {
	doc, err := specDocument(testSpec(), Config{}, nil)
	if err != nil {
		t.Fatalf("specDocument: %v", err)
	}
	if _, err := doc.Manifest(); err != nil {
		t.Fatalf("lowered document does not resolve: %v", err)
	}
}

func TestSpecDocumentDefaults(t *testing.T) {
	spec := &vm.Spec{Kernel: vm.Kernel{Path: "vmlinuz", Initrd: "initrd.img"}, Dir: t.TempDir()}
	doc, err := specDocument(spec, Config{}, nil)
	if err != nil {
		t.Fatalf("specDocument: %v", err)
	}
	if got, want := int64(doc.Machine.Memory), DefaultMemory.Mebibytes(); got != want {
		t.Errorf("default Memory = %d MiB, want %d", got, want)
	}
	if doc.Machine.VCPU <= 0 {
		t.Errorf("default VCPU = %d, want > 0", doc.Machine.VCPU)
	}
}

func TestSpecDocumentRequiresKernel(t *testing.T) {
	if _, err := specDocument(&vm.Spec{Dir: t.TempDir()}, Config{}, nil); err == nil {
		t.Fatal("expected error for missing kernel")
	}
}

func TestSpecDocumentRejectsUnalignedMemory(t *testing.T) {
	spec := &vm.Spec{Kernel: vm.Kernel{Path: "k", Initrd: "i"}, Memory: 100 * units.Kibibyte, Dir: t.TempDir()}
	if _, err := specDocument(spec, Config{}, nil); err == nil {
		t.Fatal("expected error for non-MiB-aligned memory")
	}
}

func TestSpecDocumentOverlaysBase(t *testing.T) {
	base := imanifest.DefaultDocument()
	base.Kernel.Path = "vmlinuz"
	base.Kernel.InitrdPath = "initrd.img"
	base.WorkingDir = "/work"
	base.Mounts = append(base.Mounts, imanifest.VirtioFSMountInput{
		Type:       imanifest.MountTypeVirtioFS,
		MountInput: imanifest.MountInput{Tag: "src", SourcePath: "/host/src"},
		Target:     "/workspace",
	})

	spec := &vm.Spec{
		Memory: 4096 * units.Mebibyte,
		Kernel: vm.Kernel{Path: "vmlinuz", Initrd: "initrd.img"},
		Shares: []vm.Share{
			{Tag: "src", HostPath: "ignored", GuestPath: "/elsewhere"}, // existing tag: kept from base
			{Tag: "extra", HostPath: "/host/extra", GuestPath: "/extra"},
		},
	}
	doc, err := specDocument(spec, Config{}, &base)
	if err != nil {
		t.Fatalf("specDocument: %v", err)
	}
	if got, want := int(doc.Machine.Memory), 4096; got != want {
		t.Errorf("Memory = %d MiB, want %d", got, want)
	}
	shares := doc.Mounts.VirtioFS()
	if len(shares) != 2 {
		t.Fatalf("virtiofs mounts = %+v, want base share plus appended extra", shares)
	}
	if shares[0].SourcePath != "/host/src" {
		t.Errorf("base share was replaced: %+v", shares[0])
	}
	if shares[1].Tag != "extra" {
		t.Errorf("appended share = %+v", shares[1])
	}
}

func TestHotplugDeviceMapping(t *testing.T) {
	share, err := hotplugDevice(vm.Share{Tag: "data", HostPath: "/host/data", GuestPath: "/data"})
	if err != nil {
		t.Fatalf("share: %v", err)
	}
	if share.ID != "data" || share.VirtioFS.Source != "/host/data" || share.VirtioFS.Target != "/data" {
		t.Errorf("share device = %+v", share)
	}

	disk, err := hotplugDevice(vm.Disk{Path: "/imgs/scratch.img", Format: "qcow2"})
	if err != nil {
		t.Fatalf("disk: %v", err)
	}
	if disk.Block.ImagePath != "/imgs/scratch.img" || disk.Block.Format != "qcow2" {
		t.Errorf("disk device = %+v", disk)
	}
	if strings.ContainsAny(disk.ID, "/ ") {
		t.Errorf("disk ID %q contains unsafe characters", disk.ID)
	}

	fwd, err := hotplugDevice(vm.Forward{HostAddr: "127.0.0.1:8080", GuestAddr: ":80"})
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	if len(fwd.Net.Forward) != 1 || fwd.Net.Forward[0].Proto != "tcp" {
		t.Errorf("forward device = %+v", fwd)
	}
}
