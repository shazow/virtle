<p align="center">
  <img height="260" alt="Virtle the Space Turtle" src="https://github.com/user-attachments/assets/400ba12a-6b77-4e60-b530-28183d6cacc1" />
</p>

# virtle 🐢🪐

Virtle is a VM manager for sandbox workflows.

**Status**: Beta. Used day-to-day by a few people, feedback appreciated.

Background: Originally designed to be used with [`agentspace`](https://github.com/shazow/agentspace), a NixOS-based sandbox builder, but virtle has been refined enough to act as a standalone tool.

## How does it work?

`virtle` reads a manifest, starts the required host processes, and launches
the VM backend (QEMU by default, Firecracker, or Cloud Hypervisor). For QEMU guests it also
waits for SSH readiness and attaches an active session with `--ssh`.

It also handles teardown, QMP-based shutdown, disk-backed suspend/resume, runtime vsock CID allocation, QGA-based remote commands, and more.

### Features

- Runs QEMU, Firecracker, or Cloud Hypervisor microVMs through the same CLI
  and Go interfaces.
- Networks guests in userspace with an egress policy: the internet and nothing
  on the host or its LAN by default, allow and deny by name, record every
  connection, and let the guest use secrets it never holds (see
  [docs/networking.md](docs/networking.md)).
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

### Backends

QEMU is the default. Set `backend = "firecracker"` to launch a Firecracker
microVM instead (Linux with KVM; direct kernel boot, raw disks, serial output,
a host TAP NIC, and the same lifecycle commands), or `backend =
"cloud-hypervisor"` for Cloud Hypervisor, which adds virtio-fs shares to that
list. Guest control, SSH, the virtle network, suspend, ballooning, and hotplug
are QEMU-only today. See [docs/firecracker.md](docs/firecracker.md),
[docs/cloud-hypervisor.md](docs/cloud-hypervisor.md), and the
[Firecracker](docs/recipes/firecracker/README.md) and
[Cloud Hypervisor](docs/recipes/cloud-hypervisor/README.md) recipes.

```toml
backend = "firecracker"

[kernel]
path = "vmlinux"
serial = "print"

[[mounts]]
type = "image"
source = "rootfs.ext4"
target = "/"
read_only = true
```

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
process environment is available as `.Env` on every surface, and
`{{fromFile "path"}}` reads a file (relative to the manifest's directory)
without its trailing newline.

| Surface | Template values | Injected environment |
| --- | --- | --- |
| `qemu.exec` | `HostName`, `WorkingDir`, `StateDir`, `HostOS`, `HostArch`, `HostSystem`, `.Env` | none |
| `qemu.fwd_tunnel_exec` | `Host`, `Port`, `.Env` | none; QEMU starts the command |
| `ssh.exec` | `CID`, `User`, `Destination`, `.Env` | `CID`, `USER`, `DESTINATION` |
| `mounts[type=virtiofs].virtiofs` | `Socket`, `MountSource`, `MountTag`, `CID`, `StateDir`, `.Env` | `SOCKET`, `MOUNT_SOURCE`, `MOUNT_TAG`, `CID`, `STATE_DIR`, `VIRTIOFSD_SOCKET` |
| `run[].exec` | `CID`, `StateDir`, `Workspace.GuestPath`, `Workspace.HostPath`, user vars, `.Env` | scalar top-level values only |
| `notifications.exec` | `State`, `Message`, notification context values, `.Env` | `STATE`, `MESSAGE`, normalized context values, `VIRTLE_NOTIFY_STATE`, `VIRTLE_NOTIFY_MESSAGE`, `VIRTLE_NOTIFY_CONTEXT_<KEY>` |
| `egress.secrets[].from` | `.Env`, `fromFile`; rendered when a request carries the token | none |

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

`&firecracker.Backend{}` and `&cloudhypervisor.Backend{}` take the same
`vm.Spec` and return the same `backend.Machine`; see
[docs/firecracker.md](docs/firecracker.md) and
[docs/cloud-hypervisor.md](docs/cloud-hypervisor.md) for what they support.

Put the guest on a network virtle runs, with a policy on what it may reach
(see [docs/networking.md](docs/networking.md)):

```go
policy := &egress.Policy{Rules: []egress.Rule{{Hosts: []string{"*.github.com"}, Ports: []int{443}}}}
network, err := userspace.New(userspace.Config{DNS: userspace.DNSFakeIP, Egress: policy})
defer network.Close()

b := &qemu.Backend{Network: network, RemoteControl: qemu.QGA{}}
m, err := b.Start(ctx, spec)
status, _ := m.(backend.StatusReporter).Status(ctx)
conn, err := network.DialContext(ctx, "tcp", status.Networks[0].Addr+":22")
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
