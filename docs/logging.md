# Logging

Virtle keeps command results and explicit streams separate from diagnostic
logs:

- Results such as the SSH connection hint use stdout.
- Attached SSH uses the process's stdout and stderr. An explicitly requested
  VM console uses its configured writer (stderr in the CLI).
- Logs use the standard `log/slog` logger and go to stderr.
- A terminal command error is printed once as `error: ...` after it reaches
  `main`.

The CLI uses the standard logger's default format and changes its minimum
level from the number of `-v` flags:

| Flags | Minimum level | Intended use |
| --- | --- | --- |
| none | `WARN` | Results, warnings, and errors |
| `-v` | `INFO` | Useful VM and SSH lifecycle events |
| `-vv` | `DEBUG` | Troubleshooting details and background command output |

Lifecycle messages that are useful during a normal launch are logged at
`INFO`. Detailed commands, launch statistics, allocations, and guest
diagnostics are logged at `DEBUG`. Warnings are visible at every verbosity.

The logger is created in `main` and passed into the QEMU backend at
construction. Sub-packages derive loggers carrying a `package` attribute such
as `session`, `vmm`, `ssh`, or `balloon`; they do not configure a process-global
logger. Library callers may provide their own `*slog.Logger` in `qemu.Config`.
If they do not, backend logging is discarded.

External commands are started through `internal/executor`. At `DEBUG`, an
otherwise unconnected stdout or stderr stream is recorded one line at a time,
tagged with its command and stream. Explicit command writers are preserved, so
SSH and console output are not duplicated as logs.

## Example output

The following excerpts show the same representative command at each level.
Timestamps, paths, CIDs, and rendered commands vary by launch.

With no verbosity flag, lifecycle logs are hidden. The remote command's stdout
is still connected directly:

```console
$ virtle launch --ssh -- echo ready
ready
```

Warnings would still be visible:

```text
2026/08/20 00:01:02 WARN notification hook failed package=vmm state=ready err="exit status 1"
```

With `-v`, the same result is accompanied by useful lifecycle information:

```console
$ virtle -v launch --ssh -- echo ready
2026/08/20 00:01:02 INFO loading launch manifest package=main path=manifest.toml
2026/08/20 00:01:02 INFO starting vm session package=session resume=auto ssh=true
2026/08/20 00:01:03 INFO waiting for guest agent readiness package=vmm
2026/08/20 00:01:03 INFO waiting for ssh readiness package=ssh
2026/08/20 00:01:04 INFO guest is ready package=ssh
2026/08/20 00:01:04 INFO vm started; entering foreground session package=session
2026/08/20 00:01:04 INFO ssh command package=ssh command="ssh ... echo ready"
ready
2026/08/20 00:01:04 INFO vm session ended package=session
```

With `-vv`, all of the preceding output remains, with debugging records and
background process streams added:

```console
$ virtle -vv launch --ssh -- echo ready
2026/08/20 00:01:02 INFO loading launch manifest package=main path=manifest.toml
2026/08/20 00:01:02 INFO starting vm session package=session resume=auto ssh=true
2026/08/20 00:01:02 DEBUG allocated vsock cid package=vmm cid=7
2026/08/20 00:01:02 DEBUG starting run package=vmm index=0
2026/08/20 00:01:02 DEBUG ready package=vmm command=workspace-helper stream=stdout
2026/08/20 00:01:03 DEBUG starting qemu package=vmm command="qemu-system-x86_64 ..."
2026/08/20 00:01:04 INFO guest is ready package=ssh
2026/08/20 00:01:04 INFO ssh command package=ssh command="ssh ... echo ready"
ready
2026/08/20 00:01:04 DEBUG launch stats package=vmm stats="total=2s ..."
2026/08/20 00:01:04 INFO vm session ended package=session
```

An explicitly enabled VM console or attached SSH session may interleave its
raw output with stderr logs on a terminal. Redirect stdout and stderr
separately when a stable machine-readable result stream is required.
