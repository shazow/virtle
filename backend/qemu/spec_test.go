package qemu

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	imanifest "github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/units"
	"github.com/shazow/virtle/vm"
)

func TestBackendLoggersAreConfiguredIndependently(t *testing.T) {
	var firstOutput, secondOutput bytes.Buffer
	first := &Backend{Logger: slog.New(slog.NewTextHandler(&firstOutput, nil))}
	second := &Backend{Logger: slog.New(slog.NewTextHandler(&secondOutput, nil))}

	first.logger().Info("first backend")
	second.logger().Info("second backend")

	if logs := firstOutput.String(); !strings.Contains(logs, "first backend") || strings.Contains(logs, "second backend") {
		t.Fatalf("unexpected first backend logs: %q", logs)
	}
	if logs := secondOutput.String(); !strings.Contains(logs, "second backend") || strings.Contains(logs, "first backend") {
		t.Fatalf("unexpected second backend logs: %q", logs)
	}
}

func TestBackendZeroValueDefaults(t *testing.T) {
	var defaults Backend
	if defaults.consoleOutput() != os.Stderr {
		t.Fatal("expected default console output on stderr")
	}
	if defaults.logger() == nil || defaults.logger().Enabled(context.Background(), slog.LevelError) {
		t.Fatal("expected the default logger to discard")
	}

	var foreground bytes.Buffer
	configured := &Backend{ConsoleOutput: &foreground}
	if configured.consoleOutput() != &foreground {
		t.Fatal("expected configured console output to be preserved")
	}
}

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
	doc, err := specDocument(testSpec(), &Backend{HostName: "testvm"}, nil)
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
		doc.WriteFiles[0].Mode == nil || *doc.WriteFiles[0].Mode != "0644" {
		t.Errorf("write files = %+v", doc.WriteFiles)
	}
}

func TestSpecDocumentQGASocketOverride(t *testing.T) {
	doc, err := specDocument(testSpec(), &Backend{RemoteControl: QGA{SocketPath: "custom-qga.sock"}}, nil)
	if err != nil {
		t.Fatalf("specDocument: %v", err)
	}
	if got, want := doc.QEMU.GuestAgentSocket, "custom-qga.sock"; got != want {
		t.Errorf("GuestAgentSocket = %q, want %q", got, want)
	}
}

func TestSpecDocumentHotplugPorts(t *testing.T) {
	doc, err := specDocument(testSpec(), &Backend{HotplugPorts: 3}, nil)
	if err != nil {
		t.Fatalf("specDocument: %v", err)
	}
	if got, want := doc.QEMU.HotplugPorts, 3; got != want {
		t.Errorf("QEMU.HotplugPorts = %d, want %d", got, want)
	}
	mf, err := doc.Manifest()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got, want := mf.QEMU.Hotplug.PCIEPorts, 3; got != want {
		t.Errorf("PCIEPorts = %d, want %d", got, want)
	}
}

func TestSpecDocumentAccel(t *testing.T) {
	on, off := true, false
	for _, tt := range []struct {
		accel Accel
		want  *bool
	}{
		{AccelAuto, nil},
		{AccelKVM, &on},
		{AccelTCG, &off},
	} {
		doc, err := specDocument(testSpec(), &Backend{Accel: tt.accel}, nil)
		if err != nil {
			t.Fatalf("specDocument(%q): %v", tt.accel, err)
		}
		switch {
		case tt.want == nil && doc.Machine.KVM != nil:
			t.Errorf("Accel %q: KVM = %v, want unset", tt.accel, *doc.Machine.KVM)
		case tt.want != nil && (doc.Machine.KVM == nil || *doc.Machine.KVM != *tt.want):
			t.Errorf("Accel %q: KVM = %v, want %v", tt.accel, doc.Machine.KVM, *tt.want)
		}
	}
	if _, err := specDocument(testSpec(), &Backend{Accel: "hvf"}, nil); err == nil {
		t.Fatal("expected an unsupported accelerator to be rejected")
	}
}

func TestSpecDocumentResolves(t *testing.T) {
	doc, err := specDocument(testSpec(), &Backend{}, nil)
	if err != nil {
		t.Fatalf("specDocument: %v", err)
	}
	if _, err := doc.Manifest(); err != nil {
		t.Fatalf("lowered document does not resolve: %v", err)
	}
}

func TestSpecDocumentDefaults(t *testing.T) {
	spec := &vm.Spec{Kernel: vm.Kernel{Path: "vmlinuz", Initrd: "initrd.img"}, Dir: t.TempDir()}
	doc, err := specDocument(spec, &Backend{}, nil)
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
	if _, err := specDocument(&vm.Spec{Dir: t.TempDir()}, &Backend{}, nil); err == nil {
		t.Fatal("expected error for missing kernel")
	}
}

func TestSpecDocumentRejectsUnalignedMemory(t *testing.T) {
	spec := &vm.Spec{Kernel: vm.Kernel{Path: "k", Initrd: "i"}, Memory: 100 * units.Kibibyte, Dir: t.TempDir()}
	if _, err := specDocument(spec, &Backend{}, nil); err == nil {
		t.Fatal("expected error for non-MiB-aligned memory")
	}
}

func TestSpecDocumentOverlaysBase(t *testing.T) {
	owner := "root:root"
	overwrite := true
	hostSource := "host.conf"
	writeBack := true
	diskLabel := "data"
	diskSerial := "base-serial"
	inlineText := "old content"
	inlineMode := "600"
	base := imanifest.DefaultDocument()
	base.Kernel.Path = "vmlinuz"
	base.Kernel.InitrdPath = "initrd.img"
	base.WorkingDir = "/work"
	base.StateDir = "/state"
	base.Mounts = imanifest.MountsInput{
		imanifest.VirtioFSMountInput{
			Type:       imanifest.MountTypeVirtioFS,
			MountInput: imanifest.MountInput{Tag: "old-src", SourcePath: "/host/old"},
			Target:     "/old-workspace",
			VirtioFS: imanifest.VirtioFSInput{
				Socket: "custom.sock",
				Bin:    "/custom/virtiofsd",
				Args:   []string{"--socket={{.Socket}}", "--source={{.MountSource}}", "--tag={{.MountTag}}"},
			},
		},
		imanifest.NinePMountInput{
			Type:       imanifest.MountTypeNineP,
			MountInput: imanifest.MountInput{Tag: "backend-only", SourcePath: "/host/legacy"},
			NineP:      imanifest.NinePInput{SecurityModel: "none"},
		},
		imanifest.ImageMountInput{
			Type:       imanifest.MountTypeImage,
			SourcePath: "old.img",
			ReadOnly:   true,
			Image: imanifest.ImageInput{
				Size:   64,
				FSType: "ext4",
				Format: "raw",
				Label:  &diskLabel,
				Direct: true,
				Serial: &diskSerial,
			},
		},
	}
	base.Networks[0].Forward = []imanifest.ForwardPort{
		{Proto: "tcp", From: "host", Host: "127.0.0.1:1000", Guest: "10.0.2.15:10"},
		{Proto: "tcp", From: "guest", Host: "127.0.0.1:2000", Guest: "10.0.2.15:20"},
	}
	base.WriteFiles = []imanifest.WriteFileInput{
		{
			GuestPath: "/etc/old",
			Chown:     &owner,
			Text:      &inlineText,
			Mode:      &inlineMode,
			Overwrite: &overwrite,
		},
		{
			GuestPath: "/etc/backend.conf",
			Path:      &hostSource,
			WriteBack: &writeBack,
		},
	}

	spec := &vm.Spec{
		Memory: 4096 * units.Mebibyte,
		Kernel: vm.Kernel{Path: "vmlinuz", Initrd: "initrd.img"},
		Shares: []vm.Share{{Tag: "src", HostPath: "/host/new", GuestPath: "/workspace", ReadOnly: true}},
		Disks:  []vm.Disk{{Path: "new.qcow2", Format: "qcow2", Size: 256 * units.Mebibyte}},
		Ports:  []vm.Forward{{Proto: vm.UDP, HostAddr: "127.0.0.1:8080", GuestAddr: "10.0.2.15:80"}},
		Files:  []vm.File{{GuestPath: "/etc/new", Content: strings.NewReader("new content"), Mode: 0o640}},
	}
	doc, err := specDocument(spec, &Backend{}, &base)
	if err != nil {
		t.Fatalf("specDocument: %v", err)
	}
	if got, want := int(doc.Machine.Memory), 4096; got != want {
		t.Errorf("Memory = %d MiB, want %d", got, want)
	}
	mf, err := doc.Manifest()
	if err != nil {
		t.Fatalf("resolve overlaid document: %v", err)
	}
	runs, err := mf.ResolvedRuns(3)
	if err != nil {
		t.Fatalf("resolve runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %+v, want one virtiofs helper", runs)
	}
	if got, want := runs[0].Exec, []string{"/custom/virtiofsd", "--socket=/state/custom.sock", "--source=/host/new", "--tag=src"}; !reflect.DeepEqual(got, want) {
		t.Errorf("virtiofs helper = %#v, want %#v", got, want)
	}
	if got := mf.QEMU.Devices.VirtioFS; len(got) != 1 || got[0].Tag != "src" || got[0].SocketPath != "custom.sock" {
		t.Errorf("virtiofs launch plan = %+v", got)
	}
	if got := doc.Mounts.VirtioFS(); len(got) != 1 || got[0].SourcePath != "/host/new" || got[0].Target != "/workspace" || !got[0].ReadOnly {
		t.Errorf("virtiofs guest plan = %+v", got)
	}
	if got := mf.QEMU.Devices.NineP; len(got) != 1 || got[0].Tag != "backend-only" || got[0].SourcePath != "/host/legacy" {
		t.Errorf("backend-only 9p launch plan = %+v", got)
	}
	if got := mf.ResolvedVolumes(); len(got) != 1 || got[0].ImagePath != "/work/new.qcow2" || int(got[0].Size) != 256 || got[0].FSType != "ext4" || got[0].Label != "data" || !got[0].AutoCreate {
		t.Errorf("volume plan = %+v", got)
	}
	if got := mf.QEMU.Devices.Block; len(got) != 1 || got[0].ImagePath != "new.qcow2" || got[0].Format != "qcow2" || got[0].Cache != "none" || got[0].Serial != "base-serial" || !got[0].ReadOnly {
		t.Errorf("block launch plan = %+v", got)
	}
	if got, want := mf.QEMU.Devices.Network[0].NetdevOptions, []string{
		"hostfwd=udp:127.0.0.1:8080-10.0.2.15:80",
		"guestfwd=tcp:10.0.2.15:20-cmd:nc 127.0.0.1 2000",
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("network launch plan = %#v, want %#v", got, want)
	}
	if got := doc.Networks[0].Forward; len(got) != 2 || got[1].From != "guest" {
		t.Errorf("document forwards = %+v, want replaced host forward and preserved guest forward", got)
	}
	files := mf.ResolvedWriteFiles()
	if len(files) != 2 {
		t.Fatalf("guest file plan = %+v, want one edited inline file and one backend-owned file", files)
	}
	if got := files[1]; got.GuestPath != "/etc/new" || got.Content.Kind != imanifest.WriteFileContentText || got.Content.Text != "new content" || got.Mode != "0640" || got.Chown != "root:root" || !got.Overwrite {
		t.Errorf("edited inline file = %+v", got)
	}
	if got := files[0]; got.GuestPath != "/etc/backend.conf" || got.Content.Kind != imanifest.WriteFileContentPath || got.Content.Path != "/work/host.conf" || !got.WriteBack {
		t.Errorf("backend-owned host file = %+v", got)
	}
}

func TestAdHocHotplugDevicesReceiveExecutablePlansAndDefaults(t *testing.T) {
	tmpDir := t.TempDir()
	resolver := &imanifest.Manifest{
		Paths: imanifest.Paths{
			WorkingDir: tmpDir,
			RuntimeDir: imanifest.RuntimeDir{Mode: imanifest.RuntimeDirPath, Path: filepath.Join(tmpDir, "state")},
		},
	}
	share, err := hotplugDevice(resolver, vm.Share{Tag: "data", HostPath: "/host/data", GuestPath: "/data"})
	if err != nil {
		t.Fatalf("share: %v", err)
	}
	if share.ID != "data" || share.VirtioFS.Source != "/host/data" || share.VirtioFS.Target != "/data" || share.VirtioFS.Bin != "virtiofsd" {
		t.Errorf("share device = %+v", share)
	}
	if got, want := share.VirtioFS.SocketPath, filepath.Join(tmpDir, "state", "data.sock"); got != want {
		t.Errorf("share socket = %q, want %q", got, want)
	}
	if got, want := share.VirtioFS.Args, imanifest.DefaultVirtioFSArgs(share.VirtioFS.SocketPath, "/host/data", "data"); !reflect.DeepEqual(got, want) {
		t.Errorf("share helper args = %#v, want %#v", got, want)
	}

	disk, err := hotplugDevice(resolver, vm.Disk{Path: "/imgs/scratch.img"})
	if err != nil {
		t.Fatalf("disk: %v", err)
	}
	if disk.Block.ImagePath != "/imgs/scratch.img" || disk.Block.Format != "raw" {
		t.Errorf("disk device = %+v", disk)
	}
	if strings.ContainsAny(disk.ID, "/ ") {
		t.Errorf("disk ID %q contains unsafe characters", disk.ID)
	}
	couldCollide, err := hotplugDevice(resolver, vm.Disk{Path: "/imgs/scratch_img"})
	if err != nil {
		t.Fatalf("second disk: %v", err)
	}
	if disk.ID == couldCollide.ID {
		t.Errorf("distinct disk paths produced the same ID %q", disk.ID)
	}

	fwd, err := hotplugDevice(resolver, vm.Forward{HostAddr: "127.0.0.1:8080", GuestAddr: ":80"})
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	if fwd.Net.Backend != "user" || fwd.Net.MAC == "" || len(fwd.Net.Forward) != 1 || fwd.Net.Forward[0].Proto != "tcp" || fwd.Net.Forward[0].Host != "127.0.0.1:8080" || fwd.Net.Forward[0].Guest != ":80" {
		t.Errorf("forward device = %+v", fwd)
	}
}
