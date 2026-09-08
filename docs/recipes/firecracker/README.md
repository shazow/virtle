# Firecracker appliance

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
`/input` inodes with `debugfs`. Its build and `--rebuild` comparison passed with
identical output. This verifies the rootfs image's reproducibility; the whole
guest closure has not undergone a reproducibility comparison.

The guest loads `virtio_mmio`, `virtio_blk`, `ext4`, `i8042`, and `atkbd`.
BusyBox modprobe receives i8042 options explicitly, since it does not import
module parameters from the kernel command line. BusyBox init handles
Ctrl-Alt-Del. After unmounting, it runs `reboot -f`: on x86 Firecracker exits
on that reset; `poweroff` alone leaves the VMM running. See Firecracker's
[shutdown semantics](https://github.com/firecracker-microvm/firecracker/blob/v1.15.1/FAQ.md#how-can-i-gracefully-reboot-the-guest-how-can-i-gracefully-poweroff-the-guest).

The manifest uses the common `[machine]`, `[kernel]`, and `[[mounts]]` sections.
Paths resolve against `working_dir` (default: the launch directory). Firecracker
does not expand shell variables or Go templates in executable/image paths.
Virtle marks the first disk as root; disks must already exist and use raw
format. Firecracker then appends `root=/dev/vda` and `ro` or `rw` according to
that disk's `read_only` setting. A conflicting command line such as
`root=/dev/vda1` is currently unsupported; caller parameters are passed to the
API, but Firecracker extends the effective guest command line. See the
[Firecracker root-device setup](https://github.com/firecracker-microvm/firecracker/blob/v1.15.1/src/vmm/src/builder.rs#L625-L633).
No networking, guest control, SSH, file sharing, hotplug, or suspend is enabled.

Virtle creates `.virtle` with mode 0700 and locks the manifest's VM name, shared
with QEMU. It binds `.virtle/virtle.sock` without removing any existing entry;
after an unclean host crash a stale control socket must be inspected and removed
manually before restarting. The API lives in a fresh private `virtle-fc-*`
directory under `TMPDIR`. Shutdown removes only runtime paths owned by this
launch. A programmatic backend with no `Spec.Dir` also removes its temporary
state directory. Persistent lock files remain harmless after the lock is released.

For multi-tenant deployment, supply host isolation separately: this backend
launches Firecracker directly with its default seccomp filters and does not
configure the Firecracker jailer or cgroups. Full capability limits and exact
validation results are recorded in [WIP.md](../../../WIP.md).
