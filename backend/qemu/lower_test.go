package qemu

import (
	"strings"
	"testing"

	"github.com/shazow/virtle/units"
	"github.com/shazow/virtle/vm"
)

func TestDocumentAppliesVirtleDefaultsToAnEmptySpec(t *testing.T) {
	document, err := Config{}.document(&vm.Spec{Kernel: vm.Kernel{Path: "vmlinuz", Initrd: "initrd.img"}})
	if err != nil {
		t.Fatalf("document() error = %v", err)
	}
	resolved, err := document.Manifest()
	if err != nil {
		t.Fatalf("Manifest() error = %v", err)
	}
	if got := resolved.QEMU.Memory.Size; got != 1024 {
		t.Errorf("memory = %d MiB, want virtle's 1024 MiB default", got)
	}
	if got := resolved.QEMU.BinaryPath; !strings.HasPrefix(got, "qemu-system-") {
		t.Errorf("binary = %q, want a derived qemu-system-<arch>", got)
	}
	if got := resolved.QEMU.Machine.Type; got != "microvm" {
		t.Errorf("machine = %q, want microvm", got)
	}
}

func TestDocumentCarriesSpecAndConfig(t *testing.T) {
	config := Config{
		Binary:    "/usr/bin/qemu-system-x86_64",
		Machine:   "q35",
		CPU:       "host",
		Console:   ConsolePrint,
		Seccomp:   true,
		ExtraArgs: []string{"-nodefaults"},
		HostName:  "sandbox",
		CIDRange:  CIDRange{Min: 90, Max: 99},
	}
	spec := &vm.Spec{
		CPUs:   4,
		Memory: 4 * units.Gibibyte,
		Kernel: vm.Kernel{Path: "vmlinuz", Initrd: "initrd.img", Cmdline: "quiet"},
		Ports:  []vm.Forward{{HostAddr: "127.0.0.1:2222", GuestAddr: "10.0.2.15:22"}},
		Dir:    "/run/sandbox",
	}

	document, err := config.document(spec)
	if err != nil {
		t.Fatalf("document() error = %v", err)
	}
	resolved, err := document.Manifest()
	if err != nil {
		t.Fatalf("Manifest() error = %v", err)
	}

	if got := resolved.Identity.HostName; got != "sandbox" {
		t.Errorf("host name = %q, want sandbox", got)
	}
	if got := resolved.QEMU.Memory.Size; got != 4096 {
		t.Errorf("memory = %d MiB, want 4096", got)
	}
	if got := resolved.QEMU.SMP.CPUs.QEMUValue(); got != 4 {
		t.Errorf("cpus = %d, want 4", got)
	}
	if got := resolved.QEMU.Console; got != "print" {
		t.Errorf("console = %q, want print", got)
	}
	if !resolved.QEMU.Knobs.SeccompSandbox {
		t.Error("seccomp sandbox was not enabled")
	}
	if !strings.Contains(resolved.QEMU.Kernel.Params, "quiet") {
		t.Errorf("kernel params = %q, want the spec cmdline appended", resolved.QEMU.Kernel.Params)
	}
	if got := resolved.VSock.CIDRange; got.Start != 90 || got.End != 99 {
		t.Errorf("cid range = %v, want 90-99", got)
	}
	if len(resolved.QEMU.PassthroughArgs) == 0 || resolved.QEMU.PassthroughArgs[len(resolved.QEMU.PassthroughArgs)-1] != "-nodefaults" {
		t.Errorf("passthrough args = %v, want the config's extra args", resolved.QEMU.PassthroughArgs)
	}
	if len(resolved.QEMU.Devices.Network) == 0 {
		t.Fatal("no network device resolved for the spec's port forward")
	}
	options := strings.Join(resolved.QEMU.Devices.Network[0].NetdevOptions, ",")
	if !strings.Contains(options, "hostfwd=tcp:127.0.0.1:2222-10.0.2.15:22") {
		t.Errorf("netdev options = %q, want the spec's forward", options)
	}
}

func TestDocumentRejectsSizesThatAreNotWholeMebibytes(t *testing.T) {
	_, err := Config{}.document(&vm.Spec{
		Kernel: vm.Kernel{Path: "vmlinuz", Initrd: "initrd.img"},
		Memory: units.Mebibyte + units.Kibibyte,
	})
	if err == nil {
		t.Fatal("document() error = nil, want a whole-mebibyte error")
	}
	if !strings.Contains(err.Error(), "Memory") {
		t.Errorf("document() error = %v, want it to name the offending field", err)
	}
}

func TestDocumentRequiresSpecFields(t *testing.T) {
	if _, err := (Config{}).document(nil); err == nil {
		t.Error("document(nil) error = nil, want a missing-spec error")
	}
	_, err := Config{}.document(&vm.Spec{
		Kernel: vm.Kernel{Path: "vmlinuz", Initrd: "initrd.img"},
		Shares: []vm.Share{{HostPath: "/srv"}},
	})
	if err == nil || !strings.Contains(err.Error(), "Tag") {
		t.Errorf("document() error = %v, want a missing share tag error", err)
	}
}

func TestDocumentCreatesSizedDisksOnly(t *testing.T) {
	document, err := Config{}.document(&vm.Spec{
		Kernel: vm.Kernel{Path: "vmlinuz", Initrd: "initrd.img"},
		Disks: []vm.Disk{
			{Path: "existing.img"},
			{Path: "created.img", Size: 4096 * units.Mebibyte},
		},
	})
	if err != nil {
		t.Fatalf("document() error = %v", err)
	}
	resolved, err := document.Manifest()
	if err != nil {
		t.Fatalf("Manifest() error = %v", err)
	}
	if len(resolved.Volumes) != 2 {
		t.Fatalf("resolved %d volumes, want 2", len(resolved.Volumes))
	}
	if resolved.Volumes[0].AutoCreate {
		t.Error("unsized disk was marked auto-create")
	}
	if !resolved.Volumes[1].AutoCreate {
		t.Error("sized disk was not marked auto-create")
	}
	if got := resolved.Volumes[1].Size; got != 4096 {
		t.Errorf("disk size = %d MiB, want 4096", got)
	}
}
