<p align="center">
  <img height="260" alt="Virtle the Space Turtle" src="https://github.com/user-attachments/assets/400ba12a-6b77-4e60-b530-28183d6cacc1" />
</p>

# virtle 🐢🪐

Virtle is a VM manager for sandbox workflows.

**Status**: Beta. Used day-to-day by a few people, feedback appreciated.

Background: Originally designed to be used with [`agentspace`](https://github.com/shazow/agentspace), a NixOS-based sandbox builder, but virtle has been refined enough to act as a standalone tool.

## How does it work?

`virtle` reads a manifest and launches the selected VM backend: QEMU by default,
or Firecracker with `backend = "firecracker"`. QEMU workflows can start host
helpers, wait for guest SSH readiness, and attach a session with `--ssh`.

It also handles teardown, QMP-based shutdown, disk-backed suspend/resume, runtime vsock CID allocation, QGA-based remote commands, and more.

### Features

- Runs QEMU or Firecracker microVMs through the same CLI and Go interfaces.
- Allocates block overlay images.
- Manages [`virtiofsd`](https://gitlab.com/virtio-fs/virtiofsd) daemons for virtiofs mounts.
- Provisions SSH between host and guest.
- Connects over SSH upon boot via signaling.
- Writes files between guest and host on boot or shutdown.
- Suspend and resume.
- Notification execution hooks.
- Exposes a `virtle.sock` for RPC (also usable via `virtle rpc` sub-command).
- (Experimental) Balloon memory: Auto-adjust memory available to the VM based on internal memory pressure metrics.
- (Experimental) Hotplug: Attach/detach devices during runtime (requires full VM).

Guest control, sharing, SSH, suspend, ballooning, and hotplug currently require
QEMU. Firecracker supports direct kernel boot, an optional initrd, existing raw
disks, serial output, and lifecycle/status RPCs. See the complete
[Firecracker recipe](docs/recipes/firecracker/README.md) for a Nix-built guest
and a real KVM boot/shutdown check.

### Firecracker

```toml
backend = "firecracker"

[machine]
vcpu = 1
memory = 256

[kernel]
path = "vmlinux"
initrd_path = "initrd" # optional when the kernel can boot the root disk directly
serial = "print"
params = ["console=ttyS0", "reboot=k", "panic=-1"]

[[mounts]]
type = "image"
source = "rootfs.ext4"
read_only = true
image.format = "raw"

[firecracker]
binary = "firecracker"
startup_timeout = "10s"
shutdown_timeout = "10s"
```

Use `virtle launch`, `virtle status`, and `virtle rpc shutdown`. The Go entry
point is `&firecracker.Backend{}` with the same `vm.Spec` and `backend.Machine`
interfaces as QEMU. `vm.Disk.ReadOnly` controls write access on both backends.

Firecracker requires Linux amd64/arm64 with accessible KVM and a matching guest
kernel (ELF `vmlinux` on amd64, uncompressed `Image` on arm64). There is no software
emulation fallback. The default is one vCPU and 1024 MiB. `Start` and status
`ready` indicate that the VMM accepted boot; workload readiness must be verified
inside the guest. The recipe demonstrates this distinction.

On amd64, graceful shutdown uses `SendCtrlAltDel`: the guest needs i8042/AT
keyboard drivers and an init handler that cleans up and reboots with `reboot=k`.
On arm64, this action is unavailable and shutdown currently kills the VMM with
an error. Deadlines and startup failures also force teardown. Unsupported
manifest features fail validation; they are not silently dropped. Firecracker
runs directly with its default seccomp policy; virtle does not manage a jailer,
user/network namespaces, or cgroups. See [WIP.md](WIP.md) for the full capability
and validation record.

## Usage

First, write a simple manifest and save it as `manifest.toml`. See: [./examples/manifest-*.toml](https://github.com/shazow/virtle/tree/main/examples).

Global flags:
- `virtle [--manifest=MANIFEST] ...` - When `--manifest` is omitted, `./manifest.toml` is used, falling back to `./manifest.json`. Manifests may be TOML or JSON.
- `virtle -v ...` - Show useful VM and SSH lifecycle information.
- `virtle -vv ...` - Show debugging details and output from background commands.
- `virtle --version` - Print the virtle version and exit.

Launch a VM:
- `virtle launch [--ssh] [--resume=no|auto|force] [-- <remote-cmd...>]`

Other advanced features:
- `virtle suspend`
- `virtle status`
- `virtle rpc METHOD [JSON_ARGS]`
- `virtle hotplug ID` (experimental)
- `virtle hotplug --detach ID` (experimental)

### Manifest

There are some handy sub-commands for working with manifest files:

- `virtle manifest defaults [--resolved]`
- `virtle manifest validate`
- `virtle manifest resolve`
- `virtle manifest schema`

Manifest exec arrays render each argv element as a Go `text/template`. The host
process environment is available as `.Env` on every surface.

| Surface | Template values | Injected environment |
| --- | --- | --- |
| `qemu.exec` | `HostName`, `WorkingDir`, `StateDir`, `HostOS`, `HostArch`, `HostSystem`, `.Env` | none |
| `qemu.fwd_tunnel_exec` | `Host`, `Port`, `.Env` | none; QEMU starts the command |
| `ssh.exec` | `CID`, `User`, `Destination`, `.Env` | `CID`, `USER`, `DESTINATION` |
| `mounts[type=virtiofs].virtiofs` | `Socket`, `MountSource`, `MountTag`, `CID`, `StateDir`, `.Env` | `SOCKET`, `MOUNT_SOURCE`, `MOUNT_TAG`, `CID`, `STATE_DIR`, `VIRTIOFSD_SOCKET` |
| `run[].exec` | `CID`, `StateDir`, `Workspace.GuestPath`, `Workspace.HostPath`, user vars, `.Env` | scalar top-level values only |
| `notifications.exec` | `State`, `Message`, notification context values, `.Env` | `STATE`, `MESSAGE`, normalized context values, `VIRTLE_NOTIFY_STATE`, `VIRTLE_NOTIFY_MESSAGE`, `VIRTLE_NOTIFY_CONTEXT_<KEY>` |

## Library

Virtle library docs: https://pkg.go.dev/github.com/shazow/virtle

Boot a VM, run a command, tear down:

```go
spec := &vm.Spec{
	Kernel: vm.Kernel{Path: "vmlinuz", Initrd: "initrd.img"},
	Shares: []vm.Share{{Tag: "src", HostPath: ".", GuestPath: "/workspace"}},
	Memory: 2048 * units.Mebibyte,
}
b := &qemu.Backend{
	RemoteControl: qemu.QGA{},
	Logger:        slog.Default(),
}
m, err := b.Start(ctx, spec)
if err != nil {
	log.Fatal(err)
}
defer m.Shutdown(ctx)

g, err := m.RemoteControl()
if err != nil {
	log.Fatal(err) // this VM has no guest agent
}
err = g.Run(ctx, &vm.GuestCmd{Path: "make", Dir: "/workspace", Stdout: os.Stdout})
```

Optional functionality is discovered by type assertion, as in
`database/sql/driver`:

```go
if s, ok := m.(backend.Suspender); ok {
	err = s.Suspend(ctx)
} else {
	err = m.Shutdown(ctx)
}
```

Or drive it from a manifest, as the CLI does:

```go
spec, b, err := manifest.Load(f)
m, err := b.Start(ctx, spec)
```

## License

MIT
