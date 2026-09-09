package manifest

import (
	"fmt"
	"strings"
	"testing"

	"github.com/shazow/virtle/backend/firecracker"
	"github.com/shazow/virtle/backend/qemu"
)

func TestLoadBackend(t *testing.T) {
	for _, name := range []string{"qemu", "firecracker"} {
		for _, format := range []string{"toml", "json"} {
			t.Run(name+"/"+format, func(t *testing.T) {
				input := fmt.Sprintf("backend = %q\n[kernel]\npath = 'kernel'\n[[mounts]]\ntype = 'image'\nsource = 'disk'\ntarget = '/'\nread_only = true\n", name)
				if format == "json" {
					input = fmt.Sprintf(`{"backend":%q,"kernel":{"path":"kernel"},"mounts":[{"type":"image","source":"disk","target":"/","read_only":true}]}`, name)
				}
				spec, b, err := Load(strings.NewReader(input))
				if err != nil {
					t.Fatal(err)
				}
				switch name {
				case "qemu":
					if _, ok := b.(*qemu.Backend); !ok {
						t.Fatalf("backend %T", b)
					}
				case "firecracker":
					if _, ok := b.(*firecracker.Backend); !ok {
						t.Fatalf("backend %T", b)
					}
				}
				// The root device named by target = "/" must survive the
				// lowering: Start trusts the Spec alone.
				if len(spec.Disks) != 1 || !spec.Disks[0].ReadOnly || spec.Disks[0].GuestPath != "/" {
					t.Fatalf("disks %+v", spec.Disks)
				}
			})
		}
	}
}
