# Cloud Hypervisor appliance

For a much smaller guest shared by all three backends and a rotating CLI
benchmark, see [the fast E2E fixture](../../../tests/e2e/README.md). This
recipe covers the distribution kernel, module loading, raw-disk I/O, and the
ACPI power button on [Cloud Hypervisor](https://www.cloudhypervisor.org/).

This recipe builds all boot artifacts from the repository's locked nixpkgs:
the distribution's ELF kernel (booted through its PVH entry point), a BusyBox
initrd, an ext4 raw disk containing `21`, and a virtle manifest. The guest
mounts the disk read-only, reads its input, doubles it, writes and reads back
a result in `/tmp`, and prints `VIRTLE_READY:42`. The host then requests
shutdown through `virtle rpc shutdown`, which presses the guest's ACPI power
button. BusyBox acpid runs the handler, which unmounts the disk, prints
`VIRTLE_SHUTDOWN:unmounted`, and powers off; Cloud Hypervisor exits with the
guest. The check requires a successful virtle exit and removal of its runtime
sockets.

From the repository root, including for an uncommitted working tree:

```sh
nix build path:.#checks.x86_64-linux.cloud-hypervisor --no-link -L
```

This is an actual guest boot, not a mocked API test. It requires **Linux
x86_64, accessible `/dev/kvm`, and a Nix builder advertising the `kvm` system
feature**. The derivation declares `requiredSystemFeatures = [ "kvm" ]` and
checks device access. Nix's Linux sandbox must expose KVM. A machine without
these facilities cannot run the check; it never substitutes TCG or reports a
skipped boot as success.

To run the recipe app against this checkout:

```sh
nix run path:./docs/recipes/cloud-hypervisor \
  --override-input virtle path:. --no-write-lock-file
```

The override uses this checkout and its existing `flake.lock`, including the
new backend before it is published. The app uses a temporary working directory
and runs the same readiness/shutdown protocol. To retain a guest for inspection:

```sh
nix build path:./docs/recipes/cloud-hypervisor#manifest \
  --override-input virtle path:. --no-write-lock-file -o cloud-hypervisor-manifest
nix run path:. -- --manifest ./cloud-hypervisor-manifest launch -v
# In another terminal, from the same directory:
nix run path:. -- --manifest ./cloud-hypervisor-manifest status
nix run path:. -- --manifest ./cloud-hypervisor-manifest rpc shutdown
```

`guest.nix` exports `kernel`, `initrd`, `rootfs`, `manifest`, and `run`; there
are no checked-in boot binaries. It uses the standard nixpkgs kernel and its
matching module closure, so the dependency download is larger than the guest's
runtime memory. The initrd is the guest root; the attached minimal ext4 image
supplies the workload input. The same modules support an ext4 root disk if you
adapt `/init` to switch root.

The rootfs derivation normalizes source ownership to root inside fakeroot,
fixes modes and timestamps, and checks the resulting `/`, `/lost+found`, and
`/input` inodes with `debugfs`, so the image builds reproducibly (compare with
`nix build --rebuild`). The whole guest closure makes no such claim.

The guest loads `virtio_pci`, `virtio_blk`, `ext4`, `button`, and `evdev`
where the kernel builds them as modules (the distribution kernel builds most
of them in). Every Cloud Hypervisor device is virtio-PCI, so there is no
`pci=off` here. Shutdown is the ACPI power button: the `button` driver turns
it into a key press, BusyBox `acpid` runs `/etc/acpi/PWRF/00000080`, and that
handler unmounts and runs `poweroff -f`. A power-off ends the VMM; a
`reboot` would restart the guest in place, which is also why virtle passes no
`reboot=` or `panic=` to Cloud Hypervisor guests. See the
[Cloud Hypervisor API](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/v52.0/docs/api.md)
for the `vm.power-button` request behind `rpc shutdown`.

The manifest uses the common `[machine]`, `[kernel]`, and `[[mounts]]` sections.
Paths resolve against `working_dir` (default: the launch directory). Cloud
Hypervisor does not expand shell variables or Go templates in executable/image
paths. Disks use raw format; a missing image is created as an empty ext4
filesystem when `image.create = true` and `image.size` (at least 256 MiB) are
set, as on QEMU. Virtle supplies `console=ttyS0` (for `serial = "print"`)
ahead of `kernel.params`; an image mounted at `/` (`target = "/"`) would also
get `root=/dev/vdX` and `ro` or `rw`. This recipe boots from the initrd and
attaches its disk as data, so init mounts `/dev/vda` itself and no `root=` is
passed. A `[[mounts]] type = "virtiofs"` entry would add a share served by a
`virtiofsd` virtle starts; this recipe keeps to the disk. No networking, guest
control, SSH, hotplug, or suspend is enabled.

Virtle creates `.virtle` and locks the manifest's VM name there, shared with
the other backends, then serves `.virtle/virtle.sock`; a socket left behind by
a crashed launch is replaced once the lock proves nothing else owns the
directory. The API socket lives in a fresh private `virtle-ch-*` directory
under `TMPDIR`; shutdown removes only runtime paths owned by this launch.

For multi-tenant deployment, supply host isolation separately: this backend
launches Cloud Hypervisor directly with its default seccomp filter and does not
configure landlock or cgroups. The supported feature set and its limits are
documented in [docs/cloud-hypervisor.md](../../cloud-hypervisor.md).
