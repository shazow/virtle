# Firecracker appliance

For a much smaller guest shared by all three backends and a rotating CLI
benchmark, see [the fast E2E fixture](../../../tests/e2e/README.md). This recipe
continues to cover the distribution kernel, module loading and raw-disk I/O.

This recipe builds all boot artifacts from the repository's locked nixpkgs:
an ELF kernel, a BusyBox initrd, an ext4 raw disk containing `21`, and a virtle
manifest. The guest mounts the disk read-only, reads its input, doubles it,
writes and reads back a result in `/tmp`, and prints `VIRTLE_READY:42`.
The host then requests shutdown through `virtle rpc shutdown`. Guest init
unmounts the disk, prints `VIRTLE_SHUTDOWN:unmounted`, and resets the machine.
The check requires a successful virtle exit and removal of its runtime sockets.

From the repository root, including for an uncommitted working tree:

```sh
nix build path:.#checks.x86_64-linux.firecracker --no-link -L
```

This is an actual guest boot, not a mocked API test. It requires **Linux
x86_64, accessible `/dev/kvm`, and a Nix builder advertising the `kvm` system
feature**. The derivation declares `requiredSystemFeatures = [ "kvm" ]` and
checks device access. Nix's Linux sandbox must expose KVM. A machine without
these facilities cannot run the check; it never substitutes TCG or reports a
skipped boot as success. The root flake also evaluates on aarch64-linux, but
this shutdown check is intentionally absent there because `SendCtrlAltDel`
is x86-only.

To run the recipe app against this checkout:

```sh
nix run path:./docs/recipes/firecracker \
  --override-input virtle path:. --no-write-lock-file
```

The override uses this checkout and its existing `flake.lock`, including the
new backend before it is published. The app uses a temporary working directory
and runs the same readiness/shutdown protocol. To retain a guest for inspection:

```sh
nix build path:./docs/recipes/firecracker#manifest \
  --override-input virtle path:. --no-write-lock-file -o firecracker-manifest
nix run path:. -- --manifest ./firecracker-manifest launch -v
# In another terminal, from the same directory:
nix run path:. -- --manifest ./firecracker-manifest status
nix run path:. -- --manifest ./firecracker-manifest rpc shutdown
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

The guest loads `virtio_mmio`, `virtio_blk`, `ext4`, `i8042`, and `atkbd`.
BusyBox modprobe receives i8042 options explicitly, since it does not import
module parameters from the kernel command line. BusyBox init handles
Ctrl-Alt-Del. After unmounting, it runs `reboot -f`: on x86 Firecracker exits
on that reset; `poweroff` alone leaves the VMM running. See Firecracker's
[shutdown semantics](https://github.com/firecracker-microvm/firecracker/blob/v1.15.1/FAQ.md#how-can-i-gracefully-reboot-the-guest-how-can-i-gracefully-poweroff-the-guest).

The manifest uses the common `[machine]`, `[kernel]`, and `[[mounts]]` sections.
Paths resolve against `working_dir` (default: the launch directory). Firecracker
does not expand shell variables or Go templates in executable/image paths.
Disks use raw format; a missing image is created as an empty ext4 filesystem
when `image.create = true` and `image.size` (at least 256 MiB) are set, as on
QEMU. Virtle supplies `console=ttyS0`
(for `serial = "print"`) and `reboot=k panic=-1` ahead of `kernel.params`; an
image mounted at `/` (`target = "/"`) would also get `root=/dev/vdX` and `ro`
or `rw`. This recipe boots from the initrd and attaches its disk as data, so
init mounts `/dev/vda` itself and no `root=` is passed. No networking, guest
control, SSH, file sharing, hotplug, or suspend is enabled.

Virtle creates `.virtle` and locks the manifest's VM name there, shared with
the other backends, then serves `.virtle/virtle.sock`; a socket left behind by a crashed
launch is replaced once the lock proves nothing else owns the directory. The
API socket lives in a fresh private `virtle-fc-*` directory under `TMPDIR`;
shutdown removes only runtime paths owned by this launch.

For multi-tenant deployment, supply host isolation separately: this backend
launches Firecracker directly with its default seccomp filters and does not
configure the Firecracker jailer or cgroups. The supported feature set and its
limits are documented in [docs/firecracker.md](../../firecracker.md).
