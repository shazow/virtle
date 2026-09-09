package manifest

import (
	"strings"
	"testing"
)

// The disk mounted at "/" is the root device on every backend: virtle
// passes root= for it, in mount order, and Firecracker's own root-device
// boot arguments stay off.
func TestRootDeviceNamesTheDiskMountedAtRoot(t *testing.T) {
	const mounts = `
[[mounts]]
type = "image"
source = "data.img"
[[mounts]]
type = "image"
source = "root.img"
target = "/"
read_only = true
`
	t.Run("qemu", func(t *testing.T) {
		doc, err := DecodeDocumentBytes([]byte(`[kernel]
path = "kernel"
params = ["quiet"]`+mounts), "manifest.toml")
		if err != nil {
			t.Fatal(err)
		}
		m, err := doc.Manifest()
		if err != nil {
			t.Fatal(err)
		}
		params := m.QEMU.Kernel.Params
		if !strings.Contains(params, "root=/dev/vdb ro quiet") {
			t.Fatalf("kernel params = %q, want root=/dev/vdb ro ahead of the manifest's own", params)
		}
	})
	t.Run("firecracker", func(t *testing.T) {
		doc := decodeFirecracker(t, `[kernel]
path = "vmlinux"
params = ["quiet"]`+mounts)
		m, err := doc.Manifest()
		if err != nil {
			t.Fatal(err)
		}
		if got := m.Firecracker.Kernel.Cmdline; !strings.Contains(got, "root=/dev/vdb ro quiet") {
			t.Fatalf("cmdline = %q, want root=/dev/vdb ro ahead of the manifest's own", got)
		}
		if disks := m.Firecracker.Disks; len(disks) != 2 || disks[0].Root || !disks[1].Root {
			t.Fatalf("disks = %+v, want only the second marked root", disks)
		}
	})
	t.Run("writable root", func(t *testing.T) {
		doc := decodeFirecracker(t, `[kernel]
path = "vmlinux"
[[mounts]]
type = "image"
source = "root.img"
target = "/"`)
		m, err := doc.Manifest()
		if err != nil {
			t.Fatal(err)
		}
		if got := m.Firecracker.Kernel.Cmdline; !strings.Contains(got, "root=/dev/vda rw") {
			t.Fatalf("cmdline = %q, want root=/dev/vda rw for a writable root image", got)
		}
	})
	t.Run("no root device", func(t *testing.T) {
		doc := decodeFirecracker(t, `[kernel]
path = "vmlinux"
initrd_path = "initrd"
[[mounts]]
type = "image"
source = "data.img"`)
		m, err := doc.Manifest()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(m.Firecracker.Kernel.Cmdline, "root=") || m.Firecracker.Disks[0].Root {
			t.Fatalf("data disk became the root device: %+v %q", m.Firecracker.Disks, m.Firecracker.Kernel.Cmdline)
		}
	})
}

func TestRootDeviceValidation(t *testing.T) {
	for _, tc := range []struct{ name, body, problem string }{
		{"two roots", `[[mounts]]
type = "image"
source = "a.img"
target = "/"
[[mounts]]
type = "image"
source = "b.img"
target = "/"`, "already the root device"},
		{"other mount point", `[[mounts]]
type = "image"
source = "a.img"
target = "/data"`, `only "/"`},
	} {
		for _, backend := range []string{"", `backend = "firecracker"`} {
			t.Run(tc.name+" "+backend, func(t *testing.T) {
				doc, err := DecodeDocumentBytes([]byte(backend+"\n[kernel]\npath = \"kernel\"\ninitrd_path = \"initrd\"\n"+tc.body), "manifest.toml")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := doc.Manifest(); err == nil || !strings.Contains(err.Error(), tc.problem) {
					t.Fatalf("got %v, want %q", err, tc.problem)
				}
			})
		}
	}
}
