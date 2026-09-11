# Firecracker backend

virtle can launch microVMs with [Firecracker](https://firecracker-microvm.github.io/)
instead of QEMU. Select it with `backend = "firecracker"` in a manifest, or
construct `&firecracker.Backend{}` in Go. The `vm.Spec`, `backend.Machine`,
control socket, and `virtle launch` / `status` / `rpc` commands are the same
for every backend; the differences are in what the guest gets.

For the same shape of backend with virtio-fs shares, see the
[Cloud Hypervisor backend](cloud-hypervisor.md); its guide compares the three
backends feature by feature.

The backend is early. Firecracker requires Linux on x86_64 or aarch64 with an
accessible `/dev/kvm`; there is no software-emulation fallback. Guest kernels
must match the host architecture: an ELF `vmlinux` on x86_64, an uncompressed
`Image` on aarch64.

## What works

- Direct kernel boot with an optional initrd (`[kernel]` / `vm.Kernel`).
- Raw disk images attached as virtio-block devices (`[[mounts]] type =
  "image"` / `vm.Disk`); `read_only` / `vm.Disk.ReadOnly` controls guest
  write access, and a missing image is created as an empty ext4 filesystem
  from `image.create` + `image.size` / `vm.Disk.Size` (256 MiB minimum), as
  on QEMU. The image mounted at `/` (`target = "/"` / `vm.Disk.GuestPath:
  "/"`) is the root device: virtle passes `root=/dev/vdX` and `ro` or `rw`
  for it, on QEMU alike, so a kernel boots from it without an initrd.
- The serial console (`kernel.serial = "print"` /
  `firecracker.Backend{Console: firecracker.ConsolePrint}`): printed to the
  host, and available as a `vm.Term` through `backend.ConsoleProvider`
  (`Machine.Console`) that replays what the guest printed before the attach,
  so readiness is a `bufio.Scanner` away and a BusyBox shell on ttyS0 can
  be driven from Go. QEMU offers the same.
- Lifecycle and status: `Start`, `Wait`, `Kill`, `Shutdown`, and
  `backend.StatusReporter`; over the control socket, `virtle status` and
  `virtle rpc status|wait|kill|shutdown`.
- `[[run]]` host helpers, started before the VMM and stopped after it exits,
  with the same templates as on QEMU.
- The same state directory and VM-name lock as QEMU, so a QEMU and a
  Firecracker launch of one manifest exclude each other.

The vCPU default follows QEMU's: an omitted count means every host CPU
(within Firecracker's limit of 32). An omitted memory size means 1024 MiB
(`firecracker.DefaultMemory`, the manifest default; the QEMU Go API defaults
to 2048 MiB). Small guests should set both explicitly.
`[firecracker] binary`, `startup_timeout`, and `shutdown_timeout` (or the
matching `Backend` fields) tune the VMM; `[firecracker] args` /
`Backend.ExtraArgs` append command-line arguments after virtle's own,
neither shell-expanded nor templated.

`Start` returns once Firecracker has accepted `InstanceStart`; it does not
mean the guest workload is ready. Observe readiness inside the guest: attach
`Machine.Console` and scan for a marker line, as `tests/e2e` does, or watch
`ConsoleOutput` as the [Firecracker recipe](recipes/firecracker/README.md)
does from the CLI.

## Kernel command line

virtle assembles the command line the same way it does for QEMU: console
parameters when `serial` is not `off` (`console=ttyS0`), then `reboot=k
panic=-1`, then `root=/dev/vdX` and `ro` or `rw` for the image mounted at `/`,
then the manifest's `kernel.params` / `vm.Kernel.Cmdline`, which come last
and therefore win. No drive is marked as Firecracker's own root device, so
Firecracker appends nothing itself. A boot from disks without an initrd must
name a root device this way or carry its own `root=`; virtle rejects one that
does neither instead of letting the kernel panic. Firecracker exposes no PCI
bus, so `pci=off` in your parameters skips a pointless probe.

## Shutdown

`Shutdown` sends Firecracker's `SendCtrlAltDel` action and waits for the VMM
to exit, killing it when `shutdown_timeout` (default 10s) or the caller's
context expires. The guest needs the i8042 keyboard driver, an init that
handles Ctrl-Alt-Del, and a final `reboot` (not `poweroff`, which leaves the
VMM running): with `reboot=k` from virtle, that reset exits Firecracker.
`SendCtrlAltDel` is x86-only; on aarch64, `Shutdown` kills the VMM and returns
an error wrapping `errors.ErrUnsupported`, so callers can tell the missing
capability from a guest that failed to stop.

`virtle launch` ignores `^Z` (SIGTSTP) with a warning on Firecracker machines,
since they cannot suspend; `virtle suspend` reports the missing capability.

## Not supported

Manifest settings the backend cannot honor fail validation, and `vm.Spec`
features it cannot honor fail `Start` with an error wrapping
`errors.ErrUnsupported`, rather than being silently dropped:

| Feature | Status |
| --- | --- |
| Guest control (`Machine.RemoteControl`), SSH, guest files, workspace mounts | No guest agent transport yet; see the [guest daemon design](https://github.com/shazow/virtle/pull/67). |
| Port forwards, vsock, virtle networks | The guest NIC is a host TAP device (`[[networks]] type = "tap"`, `firecracker.TAP`) that the host kernel networks; the operator owns its addressing and forwards. Frames over vsock into a virtle network follow with the guest daemon. |
| virtiofs and 9p shares, qcow2, disk cache/serial options | Raw images only, created on demand as above. [Cloud Hypervisor](cloud-hypervisor.md) has virtio-fs shares, qcow2 images, and the disk options. |
| Interactive console (`serial = "console"`) | Only `off` and `print`. |
| Suspend/resume, balloon, hotplug | Capability interfaces are not implemented. |
| `[notifications]`, `[qemu]`, `[cloud-hypervisor]` settings | Other backends'. |
| Jailer, cgroups, namespaces | Firecracker runs directly with its default seccomp filter; provide host isolation separately for multi-tenant use. |

## Runtime files

The control socket lives at `<state_dir>/virtle.sock`, next to the
`<host_name>.lock` shared with QEMU; a socket left behind by a crashed launch
is replaced once the lock proves nothing else owns the state directory. The
Firecracker API socket lives in a private `virtle-fc-*` directory under
`TMPDIR` that is removed on exit. As with QEMU, a Go backend started without
`vm.Spec.Dir` works in the process working directory and keeps its state in a
private temporary directory that is removed when the machine exits.
