package manifest

import (
	"strings"
	"testing"
)

type rejectedSetting struct{ name, toml, problem string }

// qemuOnlyRejections are the settings neither microVM backend can honor:
// they fail validation instead of being dropped.
var qemuOnlyRejections = []rejectedSetting{
	{"ssh exec", "[ssh]\nexec = ['ssh', '-v']", "ssh"},
	{"ssh ready socket", "[ssh]\nready_socket = 'ready.sock'", "ssh"},
	{"ssh autoprovision", "[ssh]\nautoprovision = true", "ssh"},
	{"vsock enabled", "[vsock]\nenabled = true", "vsock"},
	{"vsock cid range", "[vsock.cid_range]\nmin = 100\nmax = 200", "vsock"},
	{"qemu exec", "[qemu]\nexec = ['qemu-system-x86_64']", "qemu"},
	{"qemu seccomp", "[qemu]\nseccomp = true", "qemu"},
	{"qemu hotplug ports", "[qemu]\nhotplug_ports = 2", "qemu"},
	{"qemu shutdown timeout", "[qemu]\nshutdown_timeout = '5s'", "qemu"},
	{"user network", "[[networks]]\ntype = 'user'", "user networking"},
	{"virtle network", "[[networks]]\ntype = 'virtle'", "guest daemon"},
	{"tap forward", "[[networks]]\ntype = 'tap'\ntap = 'tap0'\nforward = [{ host = '127.0.0.1:2222', guest = ':22' }]", "forward"},
	{"tap without device", "[[networks]]\ntype = 'tap'", "tap is required"},
	{"write files", "[[write_files]]\nguest_path = '/etc/motd'\ntext = 'hi'", "write_files"},
	{"workspace", "[workspace]\nmount_cwd = true", "workspace"},
	{"notifications", "[notifications]\nexec = ['true']", "notifications"},
	{"balloon", "[balloon]\nenabled = true", "balloon"},
	{"hotplug", "[[hotplug.mounts]]\ntype = 'image'\nsource = 'disk.img'\nimage.serial = 'scratch'", "hotplug"},
	{"graphics", "[graphics]\nbackend = 'gtk'", "graphics"},
	{"machine cpu", "[machine]\ncpu = 'host'", "machine"},
	{"machine type", "[machine]\ntype = 'q35'", "machine"},
	{"kvm off", "[machine]\nkvm = false", "KVM"},
	{"9p mount", "[[mounts]]\ntype = '9p'\ntag = 'src'\nsource = '/src'", "mounts"},
	{"image fs", "[[mounts]]\ntype = 'image'\nsource = 'disk.img'\nimage.create = true\nimage.size = 256\nimage.fs = 'xfs'", "fs"},
	{"image too small to create", "[[mounts]]\ntype = 'image'\nsource = 'disk.img'\nimage.create = true\nimage.size = 8", "at least"},
	{"negative cpus", "[machine]\nvcpu = -1", "vcpu"},
	{"egress", "[egress]\nreach = 'rules'", "egress"},
}

// microVMAccepted are QEMU defaults spelled out explicitly, and settings
// both microVM backends honor.
var microVMAccepted = []struct{ name, toml string }{
	{"disabled balloon", "[balloon]\nenabled = false"},
	{"workspace directories", "[workspace]\nguest_dir = '/home/agent/workspace'\nhost_dir = 'workspace'"},
	{"run helper", "[[run]]\nexec = ['true', '{{.StateDir}}']"},
	{"ssh defaults spelled out", "[ssh]\nuser = 'agent'\nretry_delay = '500ms'"},
	{"vsock disabled", "[vsock]\nenabled = false"},
	{"qemu defaults spelled out", "[qemu]\nqmp_socket = 'qmp.sock'\nguest_agent_socket = 'qga.sock'"},
	{"explicit empty networks", "networks = []\n[kernel]\npath = 'vmlinux'\ninitrd_path = 'initrd'"},
	{"tap network", "[[networks]]\ntype = 'tap'\ntap = 'tap0'"},
	{"microvm", "[machine]\ntype = 'microvm'\nkvm = true"},
	{"headless", "[graphics]\nbackend = 'headless'"},
	{"image creation", "[[mounts]]\ntype = 'image'\nsource = 'scratch.img'\nimage.create = true\nimage.size = 256\nimage.label = 'scratch'"},
}

// testRejectsQEMUOnlySettings runs the shared rejection and acceptance
// tables plus a backend's own through decode.
func testRejectsQEMUOnlySettings(t *testing.T, decode func(*testing.T, string) Document, rejected []rejectedSetting, accepted []struct{ name, toml string }) {
	t.Helper()
	// An initrd keeps the disk cases clear of the root-device validation.
	const kernel = "[kernel]\npath = 'vmlinux'\ninitrd_path = 'initrd'\n"
	for _, tc := range append(append([]rejectedSetting(nil), qemuOnlyRejections...), rejected...) {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			body := tc.toml + "\n"
			if !strings.HasPrefix(tc.toml, "[kernel]") {
				body = kernel + body
			} else {
				body = strings.Replace(body, "[kernel]\n", kernel, 1)
			}
			_, err := decode(t, body).Manifest()
			if err == nil || !strings.Contains(err.Error(), tc.problem) {
				t.Fatalf("got %v, want an error mentioning %q", err, tc.problem)
			}
		})
	}
	for _, tc := range append(append([]struct{ name, toml string }(nil), microVMAccepted...), accepted...) {
		t.Run("accepts "+tc.name, func(t *testing.T) {
			body := tc.toml + "\n"
			if !strings.Contains(body, "[kernel]") {
				body = kernel + body
			}
			if _, err := decode(t, body).Manifest(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
