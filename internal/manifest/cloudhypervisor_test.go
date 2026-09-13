package manifest

import (
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/shazow/virtle/units"
)

func decodeCloudHypervisor(t *testing.T, body string) Document {
	t.Helper()
	doc, err := DecodeDocumentBytes([]byte("backend = 'cloud-hypervisor'\n"+body), "manifest.toml")
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestCloudHypervisorManifestResolves(t *testing.T) {
	doc := decodeCloudHypervisor(t, `working_dir = "/work"
[machine]
vcpu = 2
memory = 256
[kernel]
path = "vmlinux"
initrd_path = "initrd"
serial = "print"
params = ["quiet", "init=/bin/init"]
[cloud-hypervisor]
binary = "./bin/cloud-hypervisor"
shutdown_timeout = "3s"
[[mounts]]
type = "image"
source = "rootfs.ext4"
read_only = true
[[mounts]]
type = "virtiofs"
tag = "src"
source = "src"
[[mounts]]
type = "virtiofs"
tag = "cache"
source = "/var/cache"
virtiofs.socket = "/run/external.sock"
[[mounts]]
type = "image"
source = "/data/scratch.img"
image.format = "raw"
`)
	m, err := doc.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	if m.Backend != BackendCloudHypervisor || m.Identity.HostName != "virtle" || m.Firecracker != nil {
		t.Fatalf("manifest identity: backend %q host %q firecracker %v", m.Backend, m.Identity.HostName, m.Firecracker)
	}
	if got, want := m.ResolvedLockPath(), "/work/.virtle/virtle.lock"; got != want {
		t.Fatalf("lock path = %q, want %q", got, want)
	}
	console := cloudHypervisorConsole(runtime.GOARCH)
	want := &CloudHypervisor{VMM: VMM{
		Binary:          "/work/bin/cloud-hypervisor",
		StartupTimeout:  10 * time.Second,
		ShutdownTimeout: 3 * time.Second,
		CPUs:            2,
		MemoryMiB:       256,
		Kernel: BootSource{
			Path:       "/work/vmlinux",
			InitrdPath: "/work/initrd",
			Cmdline:    "console=" + console + " quiet init=/bin/init",
		},
		Disks: []VMMDisk{
			{Path: "/work/rootfs.ext4", Format: "raw", ReadOnly: true},
			{Path: "/data/scratch.img", Format: "raw"},
		},
		Console: KernelSerialPrint,
	}, Shares: []CloudHypervisorShare{
		{Tag: "src", Source: "/work/src", Socket: "/work/.virtle/src.sock"},
		{Tag: "cache", Source: "/var/cache", Socket: "/run/external.sock"},
	}}
	if !reflect.DeepEqual(m.CloudHypervisor, want) {
		t.Fatalf("resolved cloud-hypervisor = %+v, want %+v", m.CloudHypervisor, want)
	}
	// The share without a socket gets virtle's virtiofsd; the one naming a
	// socket and no daemon is served by someone else.
	runs, err := m.ResolvedRuns(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %+v, want one virtiofsd", runs)
	}
	if want := []string{"virtiofsd", "--socket-path=/work/.virtle/src.sock", "--shared-dir=/work/src", "--tag=src"}; !reflect.DeepEqual(runs[0].Exec, want) {
		t.Fatalf("virtiofsd argv = %q, want %q", runs[0].Exec, want)
	}
	cleanup, err := m.ResolvedCleanupFiles()
	if err != nil || !reflect.DeepEqual(cleanup, []string{"/work/.virtle/src.sock"}) {
		t.Fatalf("cleanup files = %q, %v", cleanup, err)
	}
}

func TestCloudHypervisorManifestDefaults(t *testing.T) {
	for name, doc := range map[string]Document{
		"decoded":      decodeCloudHypervisor(t, "working_dir = '/work'\n[kernel]\npath = 'vmlinux'\n"),
		"programmatic": {Backend: BackendCloudHypervisor, WorkingDir: "/work", Kernel: KernelInput{Path: "vmlinux"}},
		// QEMU-only defaults (the user network, the ssh command) that
		// DocumentWithDefaults fills in are virtle's, not the author's.
		"defaults applied": DocumentWithDefaults(decodeCloudHypervisor(t, "working_dir = '/work'\n[kernel]\npath = 'vmlinux'\n")),
	} {
		t.Run(name, func(t *testing.T) {
			m, err := doc.Manifest()
			if err != nil {
				t.Fatal(err)
			}
			want := &CloudHypervisor{VMM: VMM{
				Binary:          "cloud-hypervisor",
				StartupTimeout:  10 * time.Second,
				ShutdownTimeout: 10 * time.Second,
				CPUs:            defaultVMMCPUs(MaxCloudHypervisorCPUs),
				MemoryMiB:       units.MiB(1024),
				Kernel:          BootSource{Path: "/work/vmlinux"},
				Console:         KernelSerialOff,
			}}
			if !reflect.DeepEqual(m.CloudHypervisor, want) {
				t.Fatalf("resolved cloud-hypervisor = %+v, want %+v", m.CloudHypervisor, want)
			}
			if len(m.SSH.Argv) != 0 || m.QEMU.Kernel.Path != "" || len(m.Run) != 0 || len(m.CleanupFiles) != 0 {
				t.Fatalf("cloud-hypervisor manifest inherited QEMU resolution: %+v", m)
			}
		})
	}
}

func TestCloudHypervisorRejectsQEMUOnlySettings(t *testing.T) {
	testRejectsQEMUOnlySettings(t, decodeCloudHypervisor,
		[]rejectedSetting{
			{"firecracker section", "[firecracker]\nbinary = 'fc'", "firecracker"},
			{"share without tag", "[[mounts]]\ntype = 'virtiofs'\nsource = '/src'", "tag is required"},
			{"share without source", "[[mounts]]\ntype = 'virtiofs'\ntag = 'src'", "source is required"},
			{"duplicate share tag", "[[mounts]]\ntype = 'virtiofs'\ntag = 'src'\nsource = '/a'\n[[mounts]]\ntype = 'virtiofs'\ntag = 'src'\nsource = '/b'", "used twice"},
			{"too many cpus", "[machine]\nvcpu = 8193", "vcpu"},
			{"created qcow2", "[[mounts]]\ntype = 'image'\nsource = 'disk.qcow2'\nimage.format = 'qcow2'\nimage.create = true\nimage.size = 256", "raw image"},
			{"unknown format", "[[mounts]]\ntype = 'image'\nsource = 'disk.vmdk'\nimage.format = 'vmdk'", "raw and qcow2"},
		},
		[]struct{ name, toml string }{
			{"virtiofs share", "[[mounts]]\ntype = 'virtiofs'\ntag = 'src'\nsource = '/src'"},
			{"qcow2 image with serial and direct io", "[[mounts]]\ntype = 'image'\nsource = 'disk.qcow2'\nimage.format = 'qcow2'\nimage.serial = 'data'\nimage.direct = true"},
			{"interactive console", "[kernel]\npath = 'vmlinux'\nserial = 'console'"},
			{"share with its own daemon", "[[mounts]]\ntype = 'virtiofs'\ntag = 'src'\nsource = '/src'\nvirtiofs.socket = 'src.sock'\nvirtiofs.args = ['--socket-path={{.Socket}}', '--shared-dir={{.MountSource}}']"},
		})
}

// The command line carries the architecture's serial device and no reboot or
// panic policy: Cloud Hypervisor restarts a guest that resets, so the only
// exits are a power-off and the host's kill.
func TestCloudHypervisorKernelParams(t *testing.T) {
	for _, tc := range []struct {
		arch, serial string
		root, extra  []string
		want         string
	}{
		{"amd64", KernelSerialPrint, []string{"root=/dev/vda", "ro"}, []string{"quiet"}, "console=ttyS0 root=/dev/vda ro quiet"},
		{"arm64", KernelSerialPrint, nil, nil, "console=ttyAMA0"},
		{"amd64", KernelSerialOff, nil, []string{"init=/init"}, "init=/init"},
	} {
		got := cloudHypervisorKernelParams(tc.arch, tc.serial, tc.root, tc.extra)
		if got != tc.want {
			t.Errorf("%s %s: cmdline = %q, want %q", tc.arch, tc.serial, got, tc.want)
		}
		if strings.Contains(got, "reboot=") || strings.Contains(got, "panic=") {
			t.Errorf("%s: cmdline %q carries a reboot/panic policy", tc.arch, got)
		}
	}
}

// TestCloudHypervisorReadOnlyShares covers read_only on a virtiofs mount:
// virtle's default daemon arguments gain --readonly, arguments the manifest
// spells are used as written, and a share served by another daemon is left
// to that daemon.
func TestCloudHypervisorReadOnlyShares(t *testing.T) {
	const share = "working_dir = '/work'\n[kernel]\npath = 'vmlinux'\n[[mounts]]\ntype = 'virtiofs'\ntag = 'src'\nsource = '/src'\nread_only = true\n"
	for name, tc := range map[string]struct {
		extra string
		want  []string
	}{
		"default arguments":              {"", []string{"virtiofsd", "--socket-path=/work/.virtle/src.sock", "--shared-dir=/src", "--tag=src", "--readonly"}},
		"own arguments without the flag": {"virtiofs.args = ['--socket-path={{.Socket}}', '--shared-dir={{.MountSource}}']\n", []string{"virtiofsd", "--socket-path=/work/.virtle/src.sock", "--shared-dir=/src"}},
		"own arguments with the flag":    {"virtiofs.args = ['--readonly', '--socket-path={{.Socket}}']\n", []string{"virtiofsd", "--readonly", "--socket-path=/work/.virtle/src.sock"}},
	} {
		t.Run(name, func(t *testing.T) {
			m, err := decodeCloudHypervisor(t, share+tc.extra).Manifest()
			if err != nil {
				t.Fatal(err)
			}
			runs, err := m.ResolvedRuns(0)
			if err != nil || len(runs) != 1 {
				t.Fatalf("runs = %+v, %v", runs, err)
			}
			if !reflect.DeepEqual(runs[0].Exec, tc.want) {
				t.Fatalf("virtiofsd argv = %q, want %q", runs[0].Exec, tc.want)
			}
		})
	}
	m, err := decodeCloudHypervisor(t, share+"virtiofs.socket = '/run/external.sock'\n").Manifest()
	if err != nil {
		t.Fatalf("socket served elsewhere: %v", err)
	}
	if runs, err := m.ResolvedRuns(0); err != nil || len(runs) != 0 {
		t.Fatalf("runs for a socket served elsewhere = %+v, %v", runs, err)
	}
}

func TestMaxCloudHypervisorCPUsByArchitecture(t *testing.T) {
	if got := maxCloudHypervisorCPUs("amd64"); got != 8192 {
		t.Fatalf("amd64 = %d, want 8192", got)
	}
	if got := maxCloudHypervisorCPUs("arm64"); got != 255 {
		t.Fatalf("arm64 = %d, want 255", got)
	}
}

// TestCloudHypervisorDiskFormatsAndOptions covers what the VMM reads beyond
// raw images: qcow2, a serial number, and direct I/O reach the resolved disk.
func TestCloudHypervisorDiskFormatsAndOptions(t *testing.T) {
	m, err := decodeCloudHypervisor(t, "working_dir = '/work'\n[kernel]\npath = 'vmlinux'\ninitrd_path = 'initrd'\n[[mounts]]\ntype = 'image'\nsource = 'disk.qcow2'\nimage.format = 'qcow2'\nimage.serial = 'data'\nimage.direct = true\n[[mounts]]\ntype = 'image'\nsource = 'plain.img'\n").Manifest()
	if err != nil {
		t.Fatal(err)
	}
	want := []VMMDisk{
		{Path: "/work/disk.qcow2", Format: "qcow2", Serial: "data", Direct: true},
		{Path: "/work/plain.img", Format: "raw"},
	}
	if !reflect.DeepEqual(m.CloudHypervisor.Disks, want) {
		t.Fatalf("disks = %+v, want %+v", m.CloudHypervisor.Disks, want)
	}
}

// TestCloudHypervisorInteractiveConsole covers kernel.serial = "console":
// the resolved console mode is interactive and the guest still gets its
// console= parameter.
func TestCloudHypervisorInteractiveConsole(t *testing.T) {
	m, err := decodeCloudHypervisor(t, "[kernel]\npath = 'vmlinux'\nserial = 'console'\n").Manifest()
	if err != nil {
		t.Fatal(err)
	}
	if m.CloudHypervisor.Console != KernelSerialConsole || !strings.HasPrefix(m.CloudHypervisor.Kernel.Cmdline, "console=") {
		t.Fatalf("console = %q, cmdline = %q", m.CloudHypervisor.Console, m.CloudHypervisor.Kernel.Cmdline)
	}
}

// TestVMMArgsResolve covers [cloud-hypervisor] args and [firecracker] args:
// they reach the resolved configuration as given.
func TestVMMArgsResolve(t *testing.T) {
	ch, err := decodeCloudHypervisor(t, "[kernel]\npath = 'vmlinux'\n[cloud-hypervisor]\nargs = ['--seccomp', 'false']\n").Manifest()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ch.CloudHypervisor.Args, []string{"--seccomp", "false"}) {
		t.Fatalf("cloud-hypervisor args = %q", ch.CloudHypervisor.Args)
	}
	fc, err := DecodeDocumentBytes([]byte("backend = 'firecracker'\n[kernel]\npath = 'vmlinux'\n[firecracker]\nargs = ['--log-path', '/dev/null']\n"), "manifest.toml")
	if err != nil {
		t.Fatal(err)
	}
	m, err := fc.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(m.Firecracker.Args, []string{"--log-path", "/dev/null"}) {
		t.Fatalf("firecracker args = %q", m.Firecracker.Args)
	}
}
