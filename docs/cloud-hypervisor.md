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
- The same state directory and VM-name lock as the other backends.

And what Firecracker cannot do:

- virtio-fs shares (`[[mounts]] type = "virtiofs"` / `vm.Share`). virtle
  starts a [`virtiofsd`](https://gitlab.com/virtio-fs/virtiofsd) per share
  before the VMM, as it does for QEMU: the socket defaults to
  `<state_dir>/<tag>.sock` and the daemon to `virtiofsd` from `PATH`, or
  `virtiofs.socket`, `virtiofs.bin`, and `virtiofs.args` name them; a socket
  that is already live is used as it is. The guest memory is shared with
  the daemons (`memory.shared` in Cloud Hypervisor's terms) whenever there
  is a share. The guest mounts a share itself, with
  `mount -t virtiofs <tag> <dir>`; `target` / `vm.Share.GuestPath` is
  recorded but nothing in the guest acts on it yet.

The vCPU default follows QEMU's: an omitted count means every host CPU. An
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
the device itself, which needs `CAP_NET_ADMIN` unless the device already
exists and is owned by the user running virtle. Frames over vsock into a
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
| `[run]` helpers, `[notifications]`, `[qemu]`, `[firecracker]` settings | Other backends'. |
| Landlock, cgroups, namespaces | Cloud Hypervisor runs directly with its default seccomp filter; provide host isolation separately for multi-tenant use. |

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
