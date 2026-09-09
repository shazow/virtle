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
	want := &Firecracker{
		Binary:          "/work/bin/firecracker",
		StartupTimeout:  10 * time.Second,
		ShutdownTimeout: 3 * time.Second,
		CPUs:            2,
		MemoryMiB:       256,
		Kernel: FirecrackerKernel{
			Path:       "/work/vmlinux",
			InitrdPath: "/work/initrd",
			Cmdline:    "console=ttyS0 reboot=k panic=-1 quiet init=/bin/init",
		},
		Disks: []FirecrackerDisk{
			{Path: "/work/rootfs.ext4", ReadOnly: true},
			{Path: "/data/scratch.img"},
		},
		Console: KernelSerialPrint,
	}
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
			want := &Firecracker{
				Binary:          "firecracker",
				StartupTimeout:  10 * time.Second,
				ShutdownTimeout: 10 * time.Second,
				CPUs:            defaultFirecrackerCPUs(),
				MemoryMiB:       units.MiB(1024),
				Kernel:          FirecrackerKernel{Path: "/work/vmlinux", Cmdline: "reboot=k panic=-1"},
				Console:         KernelSerialOff,
			}
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
	// An initrd keeps the disk cases clear of the root-device validation.
	const kernel = "[kernel]\npath = 'vmlinux'\ninitrd_path = 'initrd'\n"
	rejected := []struct{ name, toml, problem string }{
		{"ssh exec", "[ssh]\nexec = ['ssh', '-v']", "ssh"},
		{"ssh ready socket", "[ssh]\nready_socket = 'ready.sock'", "ssh"},
		{"ssh autoprovision", "[ssh]\nautoprovision = true", "ssh"},
		{"vsock enabled", "[vsock]\nenabled = true", "vsock"},
		{"vsock cid range", "[vsock.cid_range]\nmin = 100\nmax = 200", "vsock"},
		{"qemu exec", "[qemu]\nexec = ['qemu-system-x86_64']", "qemu"},
		{"qemu seccomp", "[qemu]\nseccomp = true", "qemu"},
		{"qemu hotplug ports", "[qemu]\nhotplug_ports = 2", "qemu"},
		{"qemu shutdown timeout", "[qemu]\nshutdown_timeout = '5s'", "qemu"},
		{"network", "[[networks]]\ntype = 'user'", "networks"},
		{"write files", "[[write_files]]\nguest_path = '/etc/motd'\ntext = 'hi'", "write_files"},
		{"workspace", "[workspace]\nmount_cwd = true", "workspace"},
		{"run", "[[run]]\nexec = ['true']", "run"},
		{"notifications", "[notifications]\nexec = ['true']", "notifications"},
		{"balloon", "[balloon]\nenabled = true", "balloon"},
		{"hotplug", "[[hotplug.mounts]]\ntype = 'image'\nsource = 'disk.img'\nimage.serial = 'scratch'", "hotplug"},
		{"graphics", "[graphics]\nbackend = 'gtk'", "graphics"},
		{"machine cpu", "[machine]\ncpu = 'host'", "machine"},
		{"machine type", "[machine]\ntype = 'q35'", "machine"},
		{"kvm off", "[machine]\nkvm = false", "KVM"},
		{"interactive console", "[kernel]\nserial = 'console'", "serial"},
		{"virtiofs mount", "[[mounts]]\ntype = 'virtiofs'\ntag = 'src'\nsource = '/src'", "image mounts"},
		{"qcow2", "[[mounts]]\ntype = 'image'\nsource = 'disk.qcow2'\nimage.format = 'qcow2'", "raw"},
		{"image fs", "[[mounts]]\ntype = 'image'\nsource = 'disk.img'\nimage.create = true\nimage.size = 256\nimage.fs = 'xfs'", "fs"},
		{"image too small to create", "[[mounts]]\ntype = 'image'\nsource = 'disk.img'\nimage.create = true\nimage.size = 8", "at least"},
		{"direct io", "[[mounts]]\ntype = 'image'\nsource = 'disk.img'\nimage.direct = true", "direct"},
		{"too many cpus", "[machine]\nvcpu = 33", "vcpu"},
		{"negative cpus", "[machine]\nvcpu = -1", "vcpu"},
	}
	for _, tc := range rejected {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			body := tc.toml + "\n"
			if !strings.HasPrefix(tc.toml, "[kernel]") {
				body = kernel + body
			} else {
				body = strings.Replace(body, "[kernel]\n", kernel, 1)
			}
			_, err := decodeFirecracker(t, body).Manifest()
			if err == nil || !strings.Contains(err.Error(), tc.problem) {
				t.Fatalf("got %v, want an error mentioning %q", err, tc.problem)
			}
		})
	}
	accepted := []struct{ name, toml string }{
		{"ssh defaults spelled out", "[ssh]\nuser = 'agent'\nretry_delay = '500ms'"},
		{"vsock disabled", "[vsock]\nenabled = false"},
		{"qemu defaults spelled out", "[qemu]\nqmp_socket = 'qmp.sock'\nguest_agent_socket = 'qga.sock'"},
		{"explicit empty networks", "networks = []\n" + kernel},
		{"microvm", "[machine]\ntype = 'microvm'\nkvm = true"},
		{"headless", "[graphics]\nbackend = 'headless'"},
		{"image creation", "[[mounts]]\ntype = 'image'\nsource = 'scratch.img'\nimage.create = true\nimage.size = 256\nimage.label = 'scratch'"},
	}
	for _, tc := range accepted {
		t.Run("accepts "+tc.name, func(t *testing.T) {
			body := tc.toml + "\n"
			if !strings.Contains(body, "[kernel]") {
				body = kernel + body
			}
			if _, err := decodeFirecracker(t, body).Manifest(); err != nil {
				t.Fatal(err)
			}
		})
	}
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
		{"firecracker requires kernel", `backend = "firecracker"`, "", "kernel.path"},
		{"firecracker section needs firecracker backend", `[kernel]
path = "kernel"
initrd_path = "initrd"
[firecracker]
binary = "firecracker"`, "", "requires backend"},
		// The VM name is embedded in the state lock path of both backends.
		{"qemu host_name with separator", `host_name = "a/b"
[kernel]
path = "kernel"
initrd_path = "initrd"`, "", "host_name"},
		{"firecracker host_name dot-dot", `backend = "firecracker"
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
