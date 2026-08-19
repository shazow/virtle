<p align="center">
  <img height="260" alt="Virtle the Space Turtle" src="https://github.com/user-attachments/assets/400ba12a-6b77-4e60-b530-28183d6cacc1" />
</p>

# virtle 🐢🪐

Virtle is a VM manager for sandbox workflows.

**Status**: Beta. Used day-to-day by a few people, feedback appreciated.

Background: Originally designed to be used with [`agentspace`](https://github.com/shazow/agentspace), a NixOS-based sandbox builder, but it virtle has been refined enough to act as a standalone tool.

## How does it work?

`virtle` reads a manifest, starts the required host processes, launches QEMU,
waits for guest SSH readiness, attaches an active session with `--ssh`.

It also handles teardown, QMP-based shutdown, disk-backed suspend/resume, runtime vsock CID allocation, QGA-based remote commands, and more.

### Features

- Runs a QEMU microvm.
- Allocates block overlay images.
- Manages [`virtiofsd`](https://gitlab.com/virtio-fs/virtiofsd) daemons for virtiofs mounts.
- Provisions SSH between host and guest.
- Connects over SSH upon boot via signaling.
- Write files between guest and host on boot or shutdown.
- Suspend and resume.
- Notification execution hooks.
- Exposes a `virtle.sock` for RPC (also usable via `virtle rpc` sub-command).
- (Experimental) Balloon memory: Auto-adjust memory available to the VM based on internal memory pressure metrics.
- (Experimental) Hotplug: Attach/detach devices during runtime (requires full VM).

## Usage

First, write a simple manifest and save it as `manifest.toml`. See: [./examples/manifest.*.toml](https://github.com/shazow/virtle/tree/main/examples).

Global flags:
- `virtle [--manifest=MANIFEST] ...` - When `--manifest` is omitted, `./manifest.toml` is used by default.
- `virtle -v ...` - Show useful VM and SSH lifecycle information.
- `virtle -vv ...` - Show debugging details and output from background commands.

Without a verbosity flag, virtle only reports action consequences and warnings.
Logs are written to stderr so command results on stdout remain suitable for
piping. Explicit VM console output and an attached SSH session are always
preserved.

Launch a VM:
- `virtle launch [--ssh] [--resume=no|auto|force] [-- <remote-cmd...>]`

Other advanced features:
- `virtle suspend`
- `virtle rpc METHOD [JSON_ARGS]`
- `virtle hotplug ID` (experimental)
- `virtle hotplug --detach ID`(experimental)

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
| `mounts[type=virtiofs].virtiofs` | `Socket`, `MountSource`, `MountTag`, `.Env` | `SOCKET`, `MOUNT_SOURCE`, `MOUNT_TAG` |
| `run[].exec` | `CID`, `StateDir`, `Workspace.GuestPath`, `Workspace.HostPath`, user vars, `.Env` | scalar top-level values only |
| `notifications.exec` | `State`, `Message`, notification context values, `.Env` | `STATE`, `MESSAGE`, normalized context values |

## Library

Virtle library docs: https://pkg.go.dev/github.com/shazow/virtle

Boot a VM, run a command, tear down:

```go
spec := &vm.Spec{
	Kernel: vm.Kernel{Path: "vmlinuz", Initrd: "initrd.img"},
	Shares: []vm.Share{{Tag: "src", HostPath: ".", GuestPath: "/workspace"}},
	Memory: 2048 * units.Mebibyte,
}
b, err := qemu.New(qemu.Config{
	RemoteControl: qemu.QGA{},
	Logger:        slog.Default(),
})
if err != nil {
	log.Fatal(err)
}
inst, err := b.Start(ctx, spec)
if err != nil {
	log.Fatal(err)
}
defer backend.Shutdown(ctx, inst)

g, err := inst.RemoteControl()
if err != nil {
	log.Fatal(err) // this VM has no guest agent
}
out, err := g.Run(ctx, &vm.GuestCmd{Path: "make", Dir: "/workspace"})
fmt.Printf("exit=%d\n%s", out.ExitCode, out.Stdout)
```

Optional functionality is discovered by type assertion, as in
`database/sql/driver`:

```go
if s, ok := b.(backend.Suspender); ok {
	err = s.Suspend(ctx, inst, "")
} else {
	err = backend.Shutdown(ctx, inst)
}
```

Or drive it from a manifest, as the CLI does:

```go
spec, b, err := manifest.Load(f)
inst, err := b.Start(ctx, spec)
```

## License

MIT
