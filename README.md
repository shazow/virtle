# virtle

VM manager for sandbox workflows.

**Status**: Standalone extraction from
[`agentspace`](https://github.com/shazow/agentspace) is in progress.

`virtle` reads a manifest, starts the required host processes, launches QEMU,
waits for guest SSH readiness, and either prints an out-of-band SSH command or
attaches an active session with `--ssh`. It also handles teardown, QMP-based
shutdown, disk-backed suspend/resume, and runtime vsock CID allocation.

## Usage

```console
virtle --manifest=MANIFEST [-v|-vv] launch [--ssh] [--resume=no|auto|force] [-- <remote-cmd...>]
virtle --manifest=MANIFEST suspend
virtle --manifest=MANIFEST [-v] hotplug ID
virtle --manifest=MANIFEST [-v] hotplug --detach ID
virtle --manifest=MANIFEST rpc METHOD [JSON_ARGS]
virtle manifest defaults [--resolved]
virtle --manifest=MANIFEST manifest validate
virtle --manifest=MANIFEST manifest resolve
virtle manifest schema
```

Hand-written manifests may be JSON or TOML. The TOML examples in this
repository show a minimal manifest and a full manifest with default-valued
fields included. `virtle manifest defaults` prints the input manifest defaults
as TOML. `virtle manifest defaults --resolved` prints the internal resolved
runtime manifest defaults as TOML, using placeholder kernel paths because those
required inputs have no defaults.

`virtle manifest validate` loads, resolves, and validates a manifest. `virtle
manifest resolve` prints the fully resolved internal runtime manifest as TOML. A
JSON Schema for the manifest input format is generated at
`manifest.schema.json` and available from the binary with `virtle manifest
schema`. Regenerate it with:

```console
go run . manifest schema > manifest.schema.json
```

When `--manifest` is omitted, `virtle` checks `./manifest.toml` and then
`./manifest.json`. `--manifest` and `-v`/`--verbose` are shared options and may
be placed before or after the subcommand.

## Features

- Runs a QEMU microvm.
- Allocates block overlay images.
- Manages [`virtiofsd`](https://gitlab.com/virtio-fs/virtiofsd) daemons for
  virtiofs mounts.
- Provisions SSH between host and guest.
- Connects over SSH upon boot via signaling.
- Write files between guest and host on boot or shutdown.
- Suspend and resume.
- Notification execution hooks.
- Attaches or detaches typed hotplug devices.
- Provides `virtle rpc METHOD [JSON_ARGS]` as a convenience helper for the
  running control socket, for example `virtle rpc methods`, `virtle rpc
  guest-ps`, `virtle rpc guest-exec '{"path":"/bin/echo","args":["hello"],"captureOutput":true}'`,
  or `virtle rpc guest-read '{"path":"/etc/hostname"}'`.

## Exec Templates

Manifest exec arrays render each argv element as a Go `text/template`. The host
process environment is available as `.Env` on every surface.

| Surface | Template values | Injected environment |
| --- | --- | --- |
| `qemu.exec` | `HostName`, `WorkingDir`, `StateDir`, `HostOS`, `HostArch`, `HostSystem`, `.Env` | none |
| `qemu.fwd_tunnel_exec` | `Host`, `Port`, `.Env` | none; QEMU starts the command |
| `ssh.exec` | `CID`, `User`, `Destination`, `.Env` | `CID`, `USER`, `DESTINATION` |
| `mounts/hotplug.mounts[type=virtiofs].virtiofs` | `Socket`, `MountSource`, `MountTag`, `.Env` | `SOCKET`, `MOUNT_SOURCE`, `MOUNT_TAG` |
| `run[].exec` | `CID`, `StateDir`, `Workspace.GuestPath`, `Workspace.HostPath`, user vars, `.Env` | scalar top-level values only |
| `notifications.exec` | `State`, `Message`, notification context values, `.Env` | `STATE`, `MESSAGE`, normalized context values |

## Notes

- Runtime defaults, sockets, readiness channels, helper labels, and notification
  environment variables use the current project name consistently.
- The manifest format is intentionally narrow. It carries evaluated launch
  facts while `virtle` derives the concrete host-side QEMU policy.
- Verbose runtime logs use Go's default `log/slog` handler on stderr, with
  package identity carried as an attribute such as `package=manager`.
- Suspend/resume uses QEMU migration-to-file for disk-backed restore. The
  `SIGTSTP` signal is an internal control shim used by `virtle suspend`, not a
  terminal/job-control suspend.
- The project is developed with NixOS as the primary target. Some host
  assumptions, including the current QEMU and `virtiofsd` integration, may need
  extra work for macOS.

## License

MIT
