package manifest

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/shazow/virtle/units"
)

func decodeFirecracker(t *testing.T, body string) Document {
	t.Helper()
	doc, err := DecodeDocumentBytes([]byte("backend = 'firecracker'\n"+body), "manifest.toml")
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestFirecrackerManifestResolves(t *testing.T) {
	doc := decodeFirecracker(t, `working_dir = "/work"
[machine]
vcpu = 2
memory = 256
[kernel]
path = "vmlinux"
initrd_path = "initrd"
serial = "print"
params = ["quiet", "init=/bin/init"]
[firecracker]
binary = "./bin/firecracker"
shutdown_timeout = "3s"
[[mounts]]
type = "image"
source = "rootfs.ext4"
read_only = true
[[mounts]]
type = "image"
source = "/data/scratch.img"
image.format = "raw"
`)
	m, err := doc.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	if m.Backend != BackendFirecracker || m.Identity.HostName != "virtle" {
		t.Fatalf("manifest identity: backend %q host %q", m.Backend, m.Identity.HostName)
	}
	if got, want := m.ResolvedLockPath(), "/work/.virtle/virtle.lock"; got != want {
		t.Fatalf("lock path = %q, want %q", got, want)
	}
	socket, err := m.ResolvedControlSocketPath()
	if err != nil || socket != "/work/.virtle/virtle.sock" {
		t.Fatalf("control socket = %q, %v", socket, err)
	}
	want := &Firecracker{VMM: VMM{
		Binary:          "/work/bin/firecracker",
		StartupTimeout:  10 * time.Second,
		ShutdownTimeout: 3 * time.Second,
		CPUs:            2,
		MemoryMiB:       256,
		Kernel: BootSource{
			Path:       "/work/vmlinux",
			InitrdPath: "/work/initrd",
			Cmdline:    "console=ttyS0 reboot=k panic=-1 quiet init=/bin/init",
		},
		Disks: []VMMDisk{
			{Path: "/work/rootfs.ext4", Format: "raw", ReadOnly: true},
			{Path: "/data/scratch.img", Format: "raw"},
		},
		Console: KernelSerialPrint,
	}}
	if !reflect.DeepEqual(m.Firecracker, want) {
		t.Fatalf("resolved firecracker = %+v, want %+v", m.Firecracker, want)
	}
}

func TestFirecrackerManifestDefaults(t *testing.T) {
	for name, doc := range map[string]Document{
		"decoded":      decodeFirecracker(t, "working_dir = '/work'\n[kernel]\npath = 'vmlinux'\n"),
		"programmatic": {Backend: BackendFirecracker, WorkingDir: "/work", Kernel: KernelInput{Path: "vmlinux"}},
		// QEMU-only defaults (the user network, the ssh command) that
		// DocumentWithDefaults fills in are virtle's, not the author's.
		"defaults applied": DocumentWithDefaults(decodeFirecracker(t, "working_dir = '/work'\n[kernel]\npath = 'vmlinux'\n")),
	} {
		t.Run(name, func(t *testing.T) {
			m, err := doc.Manifest()
			if err != nil {
				t.Fatal(err)
			}
			want := &Firecracker{VMM: VMM{
				Binary:          "firecracker",
				StartupTimeout:  10 * time.Second,
				ShutdownTimeout: 10 * time.Second,
				CPUs:            defaultVMMCPUs(MaxFirecrackerCPUs),
				MemoryMiB:       units.MiB(1024),
				Kernel:          BootSource{Path: "/work/vmlinux", Cmdline: "reboot=k panic=-1"},
				Console:         KernelSerialOff,
			}}
			if !reflect.DeepEqual(m.Firecracker, want) {
				t.Fatalf("resolved firecracker = %+v, want %+v", m.Firecracker, want)
			}
			// Firecracker manifests carry no QEMU launch plan.
			if len(m.SSH.Argv) != 0 || m.QEMU.Kernel.Path != "" || m.QEMU.Devices.VSOCK.ID != "" {
				t.Fatalf("firecracker manifest inherited QEMU resolution: %+v", m)
			}
		})
	}
}

// Settings that only configure QEMU, host helpers, or guest-agent features
// fail validation instead of being dropped, while defaults spelled out
// explicitly are accepted.
func TestFirecrackerRejectsQEMUOnlySettings(t *testing.T) {
	testRejectsQEMUOnlySettings(t, decodeFirecracker,
		[]rejectedSetting{
			{"virtiofs mount", "[[mounts]]\ntype = 'virtiofs'\ntag = 'src'\nsource = '/src'", "image mounts"},
			{"qcow2", "[[mounts]]\ntype = 'image'\nsource = 'disk.qcow2'\nimage.format = 'qcow2'", "raw"},
			{"interactive console", "[kernel]\nserial = 'console'", "serial"},
			{"direct io", "[[mounts]]\ntype = 'image'\nsource = 'disk.img'\nimage.direct = true", "direct"},
			{"disk serial", "[[mounts]]\ntype = 'image'\nsource = 'disk.img'\nimage.serial = 'scratch'", "serial"},
			{"cloud-hypervisor section", "[cloud-hypervisor]\nbinary = 'ch'", "cloud-hypervisor"},
		}, nil)
	t.Run("json", func(t *testing.T) {
		doc, err := DecodeDocumentBytes([]byte(`{"backend":"firecracker","kernel":{"path":"vmlinux"},"ssh":{"ready_socket":"ready.sock"}}`), "manifest.json")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := doc.Manifest(); err == nil || !strings.Contains(err.Error(), "ssh") {
			t.Fatalf("got %v, want an ssh error", err)
		}
	})
	t.Run("programmatic", func(t *testing.T) {
		doc := Document{Backend: BackendFirecracker, Kernel: KernelInput{Path: "vmlinux"}, SSH: SSHInput{ReadySocket: "ready.sock"}}
		if _, err := doc.Manifest(); err == nil || !strings.Contains(err.Error(), "ssh") {
			t.Fatalf("got %v, want an ssh error", err)
		}
	})
}

func TestBackendSelection(t *testing.T) {
	for _, tc := range []struct{ name, input, want, problem string }{
		{"default qemu", `[kernel]
path = "kernel"
initrd_path = "initrd"`, BackendQEMU, ""},
		{"firecracker initrd optional", `backend = "firecracker"
[kernel]
path = "vmlinux"`, BackendFirecracker, ""},
		{"unknown", `backend = "typo"`, "", "backend"},
		{"qemu boots a root disk without initrd", `[kernel]
path = "kernel"
[[mounts]]
type = "image"
source = "root.img"
target = "/"`, BackendQEMU, ""},
		{"disks without initrd need a root device", `[kernel]
path = "kernel"
[[mounts]]
type = "image"
source = "data.img"`, "", "root device"},
		{"root= in params names the root", `[kernel]
path = "kernel"
params = ["root=/dev/vda1"]
[[mounts]]
type = "image"
source = "disk.img"`, BackendQEMU, ""},
		{"cloud-hypervisor initrd optional", `backend = "cloud-hypervisor"
[kernel]
path = "vmlinux"`, BackendCloudHypervisor, ""},
		{"firecracker requires kernel", `backend = "firecracker"`, "", "kernel.path"},
		{"cloud-hypervisor requires kernel", `backend = "cloud-hypervisor"`, "", "kernel.path"},
		{"firecracker section needs firecracker backend", `[kernel]
path = "kernel"
initrd_path = "initrd"
[firecracker]
binary = "firecracker"`, "", "requires backend"},
		{"cloud-hypervisor section needs cloud-hypervisor backend", `[kernel]
path = "kernel"
initrd_path = "initrd"
[cloud-hypervisor]
binary = "cloud-hypervisor"`, "", "requires backend"},
		// The VM name is embedded in the state lock path of both backends.
		{"qemu host_name with separator", `host_name = "a/b"
[kernel]
path = "kernel"
initrd_path = "initrd"`, "", "host_name"},
		{"firecracker host_name dot-dot", `backend = "firecracker"
host_name = ".."
[kernel]
path = "vmlinux"`, "", "host_name"},
		{"cloud-hypervisor host_name dot-dot", `backend = "cloud-hypervisor"
host_name = ".."
[kernel]
path = "vmlinux"`, "", "host_name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := DecodeDocumentBytes([]byte(tc.input), "")
			if err != nil {
				t.Fatal(err)
			}
			m, err := doc.Manifest()
			if tc.problem != "" {
				if err == nil || !strings.Contains(err.Error(), tc.problem) {
					t.Fatalf("got %v, want %s", err, tc.problem)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if m.Backend != tc.want {
				t.Fatalf("backend = %q, want %q", m.Backend, tc.want)
			}
		})
	}
}
