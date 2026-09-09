# Firecracker backend

virtle can launch microVMs with [Firecracker](https://firecracker-microvm.github.io/)
instead of QEMU. Select it with `backend = "firecracker"` in a manifest, or
construct `&firecracker.Backend{}` in Go. The `vm.Spec`, `backend.Machine`,
control socket, and `virtle launch` / `status` / `rpc` commands are the same
for both backends; the differences are in what the guest gets.

The backend is early. Firecracker requires Linux on x86_64 or aarch64 with an
accessible `/dev/kvm`; there is no software-emulation fallback. Guest kernels
must match the host architecture: an ELF `vmlinux` on x86_64, an uncompressed
`Image` on aarch64.

## What works

- Direct kernel boot with an optional initrd (`[kernel]` / `vm.Kernel`).
- Existing raw disk images attached as virtio-block devices (`[[mounts]]
  type = "image"` / `vm.Disk`). The first disk is the root device;
  `read_only` / `vm.Disk.ReadOnly` controls guest write access.
- Serial console output on the host (`kernel.serial = "print"` /
  `firecracker.Backend{Console: firecracker.ConsolePrint}`).
- Lifecycle and status: `Start`, `Wait`, `Kill`, `Shutdown`, and
  `backend.StatusReporter`; over the control socket, `virtle status` and
  `virtle rpc status|wait|kill|shutdown`.
- The same state directory and VM-name lock as QEMU, so a QEMU and a
  Firecracker launch of one manifest exclude each other.

Defaults follow QEMU's: an omitted vCPU count means every host CPU (within
Firecracker's limit of 32), and an omitted memory size means 1024 MiB
(`firecracker.DefaultMemory`). Small guests should set both explicitly.
`[firecracker] binary`, `startup_timeout`, and `shutdown_timeout` (or the
matching `Backend` fields) tune the VMM.

`Start` returns once Firecracker has accepted `InstanceStart`; it does not
mean the guest workload is ready. Observe readiness inside the guest (for
example, a marker line on the serial console), as the
[Firecracker recipe](recipes/firecracker/README.md) does.

## Kernel command line

virtle assembles the command line the same way it does for QEMU: console
parameters when `serial` is not `off` (`console=ttyS0`), then `reboot=k
panic=-1`, then the manifest's `kernel.params` / `vm.Kernel.Cmdline`.
Firecracker itself appends `root=/dev/vda` and `ro` or `rw` for the first disk;
a conflicting `root=` in your parameters is not supported. Firecracker exposes
no PCI bus, so `pci=off` in your parameters skips a pointless probe.

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
| Networking, port forwards, vsock | No TAP or vsock device is configured. |
| virtiofs and 9p shares, disk creation, qcow2, disk cache/serial options | Raw, existing images only. |
| Interactive console (`serial = "console"`) | Only `off` and `print`. |
| Suspend/resume, balloon, hotplug | Capability interfaces are not implemented. |
| `[run]` helpers, `[notifications]`, `[qemu]` settings | QEMU-only. |
| Jailer, cgroups, namespaces | Firecracker runs directly with its default seccomp filter; provide host isolation separately for multi-tenant use. |

## Runtime files

The control socket lives at `<state_dir>/virtle.sock`, next to the
`<host_name>.lock` shared with QEMU; a socket left behind by a crashed launch
is replaced once the lock proves nothing else owns the state directory. The
Firecracker API socket lives in a private `virtle-fc-*` directory under
`TMPDIR` that is removed on exit. As with QEMU, a Go backend started without
`vm.Spec.Dir` works in the process working directory and keeps its state in a
private temporary directory that is removed when the machine exits.
