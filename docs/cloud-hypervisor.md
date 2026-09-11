# Cloud Hypervisor backend

virtle can launch microVMs with
[Cloud Hypervisor](https://www.cloudhypervisor.org/) instead of QEMU or
Firecracker. Select it with `backend = "cloud-hypervisor"` in a manifest, or
construct `&cloudhypervisor.Backend{}` in Go. The `vm.Spec`, `backend.Machine`,
control socket, and `virtle launch` / `status` / `rpc` commands are the same
for every backend; the differences are in what the guest gets.

Cloud Hypervisor requires Linux on x86_64 or aarch64 with an accessible
`/dev/kvm`; there is no software-emulation fallback. Every device is
virtio-PCI, so guest kernels need PCI and ACPI support besides the usual
virtio drivers: an ELF `vmlinux` with the PVH entry point (`CONFIG_PVH`, which
distribution kernels have) or a `bzImage` on x86_64, an uncompressed `Image`
on aarch64.

## What works

Everything the [Firecracker backend](firecracker.md) does, the same way:

- Direct kernel boot with an optional initrd (`[kernel]` / `vm.Kernel`).
- Raw disk images attached as virtio-block devices (`[[mounts]] type =
  "image"` / `vm.Disk`), created on demand from `image.create` + `image.size`
  / `vm.Disk.Size`, with the image mounted at `/` as the root device.
- The serial console (`kernel.serial = "print"` /
  `cloudhypervisor.Backend{Console: cloudhypervisor.ConsolePrint}`), printed
  to the host and available as a `vm.Term` through `backend.ConsoleProvider`.
  Cloud Hypervisor reads serial input only from a terminal, so virtle gives
  it a pseudo-terminal for its standard streams rather than pipes; what the
  guest prints and what a `Term` types cross it unchanged.
- Lifecycle and status: `Start`, `Wait`, `Kill`, `Shutdown`, and
  `backend.StatusReporter`; over the control socket, `virtle status` and
  `virtle rpc status|wait|kill|shutdown`.
- A host TAP NIC (`[[networks]] type = "tap"` / `cloudhypervisor.TAP`).
- `[[run]]` host helpers, started before the VMM and stopped after it exits,
  with the same templates as on QEMU.
- The same state directory and VM-name lock as the other backends.

And what Firecracker cannot do:

- virtio-fs shares (`[[mounts]] type = "virtiofs"` / `vm.Share`). virtle
  starts a [`virtiofsd`](https://gitlab.com/virtio-fs/virtiofsd) per share
  before the VMM, as it does for QEMU: a mount that names no socket gets
  `<state_dir>/<tag>.sock` and `virtiofsd` from `PATH`; `virtiofs.bin` and
  `virtiofs.args` choose another daemon; a mount that names only
  `virtiofs.socket` is served by something else, which virtle waits for but
  does not start. `read_only` / `vm.Share.ReadOnly` makes the daemon refuse
  guest writes (`--readonly`), so it needs a daemon virtle starts, or
  `virtiofs.args` that carry the flag; otherwise the manifest fails
  validation rather than attaching the share writable. The guest memory is
  shared with the daemons (`memory.shared` in Cloud Hypervisor's terms)
  whenever there is a share. The guest mounts a share itself, with
  `mount -t virtiofs <tag> <dir>`; `target` / `vm.Share.GuestPath` is
  accepted but nothing in the guest acts on it.

The vCPU default follows QEMU's: an omitted count means every host CPU, and
the bound is Cloud Hypervisor's own (8192 on x86_64, 255 on aarch64). An
omitted memory size means 1024 MiB (`cloudhypervisor.DefaultMemory`, the
manifest default; the QEMU Go API defaults to 2048 MiB). Small guests should
set both explicitly. `[cloud-hypervisor] binary`, `startup_timeout`, and
`shutdown_timeout` (or the matching `Backend` fields) tune the VMM; the
startup timeout also bounds the wait for the share daemons' sockets.

`Start` returns once Cloud Hypervisor has accepted the boot request; it does
not mean the guest workload is ready. Observe readiness inside the guest:
attach `Machine.Console` and scan for a marker line, as `tests/e2e` does, or
watch `ConsoleOutput` as the
[Cloud Hypervisor recipe](recipes/cloud-hypervisor/README.md) does from the
CLI.

## Kernel command line

virtle assembles the command line as it does for the other backends, with
one difference: console parameters when `serial` is not `off` (`console=ttyS0`
on x86_64, `console=ttyAMA0` on aarch64), then `root=/dev/vdX` and `ro` or
`rw` for the image mounted at `/`, then the manifest's `kernel.params` /
`vm.Kernel.Cmdline`, which come last and therefore win. There is no
`reboot=` / `panic=` policy: Cloud Hypervisor restarts a guest that resets,
so `panic=-1` would turn a kernel panic into a boot loop. A panicking guest
halts instead until `Shutdown`'s timeout or `Kill` ends it. No drive is marked
as the VMM's own root device, and a boot from disks without an initrd must
name a root device or carry its own `root=`, as on Firecracker.

## Shutdown

`Shutdown` presses the guest's ACPI power button (`vm.power-button`) and waits
for the VMM to exit, which Cloud Hypervisor does once the guest powers off;
it kills the VMM when `shutdown_timeout` (default 10s) or the caller's context
expires first, with no other rung in between. The guest needs ACPI button
support and something that turns the event into a power-off: the tiny power
button driver (`CONFIG_ACPI_TINY_POWER_BUTTON`, which signals init directly;
the e2e fixture does this with SIGUSR2 for BusyBox init), BusyBox `acpid`
with a handler at `/etc/acpi/PWRF/00000080` (the recipe), or systemd-logind
on a distribution. The guest's path must end in a power-off, not a reboot: a
reboot restarts the guest in place and `Shutdown` times out.

`virtle launch` ignores `^Z` (SIGTSTP) with a warning on Cloud Hypervisor
machines, since they cannot suspend; `virtle suspend` reports the missing
capability.

## Networking

The guest NIC, when there is one, is a host TAP device (`[[networks]] type =
"tap"` with `tap = "tap0"`, or `cloudhypervisor.TAP{Name: "tap0"}`) that the
host kernel networks; the operator owns its addressing and forwards, so
`Spec.Ports` and `[[networks.forward]]` are rejected. Cloud Hypervisor opens
the device itself and brings it up, which needs `CAP_NET_ADMIN` unless the
device already exists, is owned by the user running virtle, and is already
up; a device it creates gets `192.168.249.1/24` on the host side, an existing
one keeps its addresses. Frames over vsock into a
virtle network follow with the guest daemon, as on Firecracker; see
[docs/networking.md](networking.md).

## Not supported

Manifest settings the backend cannot honor fail validation, and `vm.Spec`
features it cannot honor fail `Start` with an error wrapping
`errors.ErrUnsupported`, rather than being silently dropped:

| Feature | Status |
| --- | --- |
| Guest control (`Machine.RemoteControl`), SSH, guest files, workspace mounts | No guest agent transport yet; see the [guest daemon design](https://github.com/shazow/virtle/pull/67). |
| Port forwards, vsock, virtle networks | The NIC is a host TAP device; see above. |
| 9p shares, qcow2, disk cache/serial options | virtio-fs shares and raw images only. |
| Interactive console (`serial = "console"`) | Only `off` and `print`. |
| Suspend/resume, balloon, hotplug | Capability interfaces are not implemented. |
| `[notifications]`, `[qemu]`, `[firecracker]` settings | Other backends'. |
| Landlock, cgroups, namespaces | Cloud Hypervisor runs directly with its default seccomp filter; provide host isolation separately for multi-tenant use. |

## Parity with QEMU and Firecracker

What each backend offers today, and, where Cloud Hypervisor has the
capability but virtle does not wire it yet, what would close the gap:

| Feature | QEMU | Firecracker | Cloud Hypervisor |
| --- | --- | --- | --- |
| Direct kernel boot, initrd, root device at `/` | yes | yes | yes |
| Disk images | raw and qcow2, created on demand, `image.serial` and `image.direct` | raw, created on demand | raw, created on demand (the VMM reads qcow2 and honors serial and direct; not wired) |
| Shares | virtio-fs and 9p | no | virtio-fs |
| Serial console printed and as a `vm.Term` | yes | yes | yes, over a pseudo-terminal |
| Interactive console (`serial = "console"`) | yes | no | no |
| Networking | `user`, `virtle`, `tap` | `tap` | `tap` |
| Port forwards, egress policy, host-side dialing | yes | no | no |
| Guest control, SSH, guest files, workspace, `write_files` | yes (qemu-guest-agent) | no | no (the VMM has vsock for the guest daemon) |
| Graceful shutdown | guest agent, then QMP quit | Ctrl-Alt-Del (x86_64 only) | ACPI power button |
| Suspend and resume | yes | no (the VMM has a snapshot API) | no (`vm.snapshot` and `vm.restore` exist) |
| Memory resize | balloon | no | no (a balloon, or `hotplug_size` with `vm.resize`, exist) |
| Hotplug of disks, shares, forwards | yes, with ports reserved at boot | no | no (`vm.add-disk` and `vm.add-fs` exist and need no reserved ports) |
| `[[run]]` host helpers | yes | yes | yes |
| `[notifications]` hooks | yes | no | no |
| Graphics, CPU model, machine type, extra VMM arguments | yes | no | no |
| Accelerators and hosts | KVM, HVF, TCG; Linux and macOS | KVM; Linux x86_64 and aarch64 | KVM; Linux x86_64 and aarch64 |
| VMM sandboxing | optional seccomp | none (virtle does not run the jailer) | the VMM's own seccomp filter |

## Runtime files

The control socket lives at `<state_dir>/virtle.sock`, next to the
`<host_name>.lock` shared with the other backends; a socket left behind by a
crashed launch is replaced once the lock proves nothing else owns the state
directory. Share sockets live at `<state_dir>/<tag>.sock` unless
`virtiofs.socket` says otherwise, and are removed with the machine. The Cloud
Hypervisor API socket lives in a private `virtle-ch-*` directory under
`TMPDIR` that is removed on exit. As with the other backends, a Go backend
started without `vm.Spec.Dir` works in the process working directory and keeps
its state in a private temporary directory that is removed when the machine
exits.
