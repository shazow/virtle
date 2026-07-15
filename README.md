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
- `virtle [-v|-vv] ...` - Verbose and very verbose flags, recommended to understand what virtle is doing.

Launch a VM:
- `virtle launch [--ssh] [--resume=no|auto|force] [-- <remote-cmd...>]`

Other advanced features:
- `virtle suspend`
- `virtle rpc METHOD [JSON_ARGS]`
- `virtle rpc qmp JSON_MESSAGE` - Forward one request through the launch-owned QMP connection.
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

## License

MIT
