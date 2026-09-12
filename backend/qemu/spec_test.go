package qemu

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	imanifest "github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/units"
	"github.com/shazow/virtle/vm"
	"github.com/shazow/virtle/vmnet"
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

func TestBackendOutputDefaultsAndOverrides(t *testing.T) {
	defaults := &Backend{}
	if defaults.consoleOutput() != os.Stderr {
		t.Fatal("expected default console output on stderr")
	}

	var foreground bytes.Buffer
	configured := &Backend{
		ConsoleOutput: &foreground,
	}
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
		doc.WriteFiles[0].Mode == nil || *doc.WriteFiles[0].Mode != "644" {
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
	if got, want := doc.Machine.Memory, DefaultMemory.Mebibytes(); got != want {
		t.Errorf("default Memory = %d MiB, want %d", got, want)
	}
	if doc.Machine.VCPU <= 0 {
		t.Errorf("default VCPU = %d, want > 0", doc.Machine.VCPU)
	}
}

func TestSpecDocumentAcceleration(t *testing.T) {
	for _, test := range []struct {
		name  string
		accel Accel
		cpu   string
		kvm   bool
	}{
		{name: "kvm", accel: AccelKVM, cpu: "host,+x2apic,-sgx", kvm: true},
		{name: "tcg", accel: AccelTCG, cpu: "max,+x2apic", kvm: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			doc, err := specDocument(testSpec(), &Backend{Accel: test.accel}, nil)
			if err != nil {
				t.Fatalf("specDocument: %v", err)
			}
			doc.Host = imanifest.HostInput{OS: "linux", Arch: "x86_64", System: "x86_64-linux"}
			mf, err := doc.Manifest()
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if got := mf.QEMU.CPU.Model; got != test.cpu {
				t.Errorf("CPU model = %q, want %q", got, test.cpu)
			}
			if got := mf.QEMU.CPU.EnableKVM; got != test.kvm {
				t.Errorf("KVM = %v, want %v", got, test.kvm)
			}
			if got := mf.QEMU.Machine.Options; !slices.Contains(got, "accel="+string(test.accel)) {
				t.Errorf("machine options = %v, want accel=%s", got, test.accel)
			}
			if test.accel == AccelTCG {
				if got := mf.QEMU.Machine.Options; !slices.Contains(got, "pic=on") || !slices.Contains(got, "pit=on") {
					t.Errorf("TCG machine options = %v, want legacy timers enabled", got)
				}
			}
		})
	}
}

// A Spec without Dir works in the process working directory, as for
// exec.Cmd.Dir, while its runtime state goes to the directory Start
// created for it alone.
func TestResolveSpecWithoutDirUsesProcessWorkingDirectory(t *testing.T) {
	t.Chdir(t.TempDir())
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	mf, err := (&Backend{}).resolveSpec(&vm.Spec{Kernel: vm.Kernel{Path: "vmlinuz", Initrd: "initrd.img"}}, state, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("resolveSpec: %v", err)
	}
	if got := mf.Paths.WorkingDir; got != cwd {
		t.Errorf("working dir = %q, want the process working directory %q", got, cwd)
	}
	if got := mf.ResolvedPersistenceStateDir(); got != state {
		t.Errorf("state dir = %q, want %q", got, state)
	}
	if got := mf.ResolvedLockPath(); filepath.Dir(got) != state {
		t.Errorf("lock path = %q, want it under the state directory %q", got, state)
	}
}

// TestResumeRequiresDir: the matching Suspend rule lives in the VM runtime
// (vmm.TestStartVMRefusesSuspendWithEphemeralState), where the control
// socket's suspend request is refused as well.
func TestResumeRequiresDir(t *testing.T) {
	_, err := (&Backend{}).Resume(t.Context(), &vm.Spec{Kernel: vm.Kernel{Path: "vmlinuz", Initrd: "initrd.img"}})
	if err == nil || !strings.Contains(err.Error(), "Dir") {
		t.Errorf("Resume without Dir = %v, want an error naming vm.Spec.Dir", err)
	}
}

func TestSpecDocumentRequiresKernel(t *testing.T) {
	if _, err := specDocument(&vm.Spec{Dir: "/work"}, &Backend{}, nil); err == nil {
		t.Fatal("expected error for missing kernel")
	}
}

func TestSpecDocumentRejectsUnalignedMemory(t *testing.T) {
	spec := &vm.Spec{Kernel: vm.Kernel{Path: "k", Initrd: "i"}, Memory: 100 * units.Kibibyte, Dir: "/work"}
	if _, err := specDocument(spec, &Backend{}, nil); err == nil {
		t.Fatal("expected error for non-MiB-aligned memory")
	}
}

// TestSpecRejectsNonRootGuestPath: only "/" (the root device) has a meaning
// without a guest agent, so any other mount point is refused rather than
// silently ignored, as on Firecracker.
func TestSpecRejectsNonRootGuestPath(t *testing.T) {
	spec := testSpec()
	spec.Disks = []vm.Disk{{Path: "data.img", GuestPath: "/data"}}
	_, err := specDocument(spec, &Backend{}, nil)
	if !errors.Is(err, errors.ErrUnsupported) || !strings.Contains(err.Error(), "GuestPath") {
		t.Fatalf("GuestPath /data = %v, want ErrUnsupported naming vm.Disk.GuestPath", err)
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
				Args:   []string{"--socket={{.Socket}}", "--source={{.MountSource}}", "--tag={{.MountTag}}", "--readonly"},
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
		Disks:  []vm.Disk{{Path: "new.qcow2", Format: "qcow2", Size: 256 * units.Mebibyte, ReadOnly: true}},
		Ports:  []vm.Forward{{Proto: "udp", HostAddr: "127.0.0.1:8080", GuestAddr: "10.0.2.15:80"}},
		Files:  []vm.File{{GuestPath: "/etc/new", Content: strings.NewReader("new content"), Mode: 0o640}},
	}
	baseForwards := slices.Clone(base.Networks[0].Forward)
	baseMachineOptions := maps.Clone(base.QEMU.MachineOptions)
	doc, err := specDocument(spec, &Backend{}, &base)
	if err != nil {
		t.Fatalf("specDocument: %v", err)
	}
	if got, want := int(doc.Machine.Memory), 4096; got != want {
		t.Errorf("Memory = %d MiB, want %d", got, want)
	}
	if !reflect.DeepEqual(base.Networks[0].Forward, baseForwards) || !reflect.DeepEqual(base.QEMU.MachineOptions, baseMachineOptions) {
		t.Errorf("specDocument mutated the base document: forwards %+v, machine options %+v", base.Networks[0].Forward, base.QEMU.MachineOptions)
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
	if got, want := runs[0].Exec, []string{"/custom/virtiofsd", "--socket=/state/custom.sock", "--source=/host/new", "--tag=src", "--readonly"}; !reflect.DeepEqual(got, want) {
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
	if got := files[1]; got.GuestPath != "/etc/new" || got.Content.Kind != imanifest.WriteFileContentText || got.Content.Text != "new content" || got.Mode != "640" || got.Chown != "root:root" || !got.Overwrite {
		t.Errorf("edited inline file = %+v", got)
	}
	if got := files[0]; got.GuestPath != "/etc/backend.conf" || got.Content.Kind != imanifest.WriteFileContentPath || got.Content.Path != "/work/host.conf" || !got.WriteBack {
		t.Errorf("backend-owned host file = %+v", got)
	}
}

// fakeVMNet stands in for a vmnet.Network that is never attached to.
type fakeVMNet struct{}

func (fakeVMNet) MTU() int { return 1500 }

func (fakeVMNet) Attach(context.Context, vmnet.Link, vmnet.AttachOptions) (vmnet.Port, error) {
	return nil, errors.New("not attached in this test")
}

func TestSpecDocumentAppliesLink(t *testing.T) {
	network := fakeVMNet{}
	for name, tc := range map[string]struct {
		cfg      Backend
		wantType string
		wantTap  string
		wantErr  error
		anyErr   bool
	}{
		"default":                {cfg: Backend{}, wantType: "user"},
		"network selects stream": {cfg: Backend{Network: network}, wantType: "virtle"},
		"stream":                 {cfg: Backend{Network: network, Link: Stream{}}, wantType: "virtle"},
		"user":                   {cfg: Backend{Link: User{}}, wantType: "user"},
		"tap":                    {cfg: Backend{Link: TAP{Name: "tap0"}}, wantType: "tap", wantTap: "tap0"},
		"stream without network": {cfg: Backend{Link: Stream{}}, wantErr: errors.ErrUnsupported},
		"user with network":      {cfg: Backend{Network: network, Link: User{}}, wantErr: errors.ErrUnsupported},
		"tap with network":       {cfg: Backend{Network: network, Link: TAP{Name: "tap0"}}, wantErr: errors.ErrUnsupported},
		"tap without name":       {cfg: Backend{Link: TAP{}}, anyErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			doc, err := specDocument(testSpec(), &tc.cfg, nil)
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("error = %v, want %v", err, tc.wantErr)
				}
				return
			case tc.anyErr:
				if err == nil {
					t.Fatal("specDocument succeeded")
				}
				return
			case err != nil:
				t.Fatalf("specDocument: %v", err)
			}
			if len(doc.Networks) != 1 || doc.Networks[0].Type != tc.wantType || doc.Networks[0].Tap != tc.wantTap {
				t.Fatalf("networks = %+v, want one of type %q tap %q", doc.Networks, tc.wantType, tc.wantTap)
			}
		})
	}

	// A Network-backed document resolves to a managed NIC carrying the
	// Spec's forwards for the port, with no slirp options.
	doc, err := specDocument(testSpec(), &Backend{Network: network}, nil)
	if err != nil {
		t.Fatal(err)
	}
	mf, err := doc.Manifest()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	devices := mf.QEMU.Devices.Network
	if len(devices) != 1 || !devices[0].Managed || devices[0].Backend != "stream" || len(devices[0].NetdevOptions) != 0 {
		t.Fatalf("network devices = %+v, want one managed stream device", devices)
	}
	if want := []imanifest.HotplugForward{{Proto: "tcp", Host: "127.0.0.1:8080", Guest: "10.0.2.15:80"}}; !reflect.DeepEqual(devices[0].Forward, want) {
		t.Fatalf("forwards = %+v, want %+v", devices[0].Forward, want)
	}
}

func TestResolveSpecKeepsManifestNetworkTypes(t *testing.T) {
	doc, err := imanifest.DecodeDocumentBytes([]byte(`
[kernel]
path = "vmlinuz"

[[networks]]
id = "managed"
type = "virtle"

[[networks]]
id = "default"

[[networks]]
id = "slirp"
type = "user"
forward = [{ from = "guest", host = "127.0.0.1:2000", guest = "10.0.2.15:20" }]

[[networks]]
id = "host"
type = "tap"
tap = "tap0"
`), "")
	if err != nil {
		t.Fatal(err)
	}
	// The loader sets Network for the virtle NIC and leaves Link nil. Use
	// the same backend construction and resolution path that Start uses.
	b := NewBackendFromDocument(doc, Backend{Network: fakeVMNet{}}).(*Backend)
	mf, err := b.resolveSpec(&vm.Spec{}, "", b.logger())
	if err != nil {
		t.Fatalf("resolve mixed network manifest: %v", err)
	}
	devices := mf.QEMU.Devices.Network
	want := []struct {
		id, backend string
		managed     bool
		options     []string
	}{
		{id: "managed", backend: "stream", managed: true},
		{id: "default", backend: "user"},
		{id: "slirp", backend: "user", options: []string{"guestfwd=tcp:10.0.2.15:20-cmd:nc 127.0.0.1 2000"}},
		{id: "host", backend: "tap", options: []string{"ifname=tap0", "script=no", "downscript=no"}},
	}
	if len(devices) != len(want) {
		t.Fatalf("network devices = %+v, want %d", devices, len(want))
	}
	for i, expected := range want {
		got := devices[i]
		if got.ID != expected.id || got.Backend != expected.backend || got.Managed != expected.managed || !slices.Equal(got.NetdevOptions, expected.options) {
			t.Errorf("network %d = %+v, want %+v", i, got, expected)
		}
	}
}
