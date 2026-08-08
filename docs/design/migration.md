# Migration: impacted consumer surfaces

Status: proposal, companion to [library-api.md](library-api.md)
(refs [#66](https://github.com/shazow/virtle/issues/66))

This document inventories every surface a consumer of virtle touches today —
CLI users, manifest authors, control-socket clients, guest-image builders,
and would-be Go library consumers — and states how each one changes (or
doesn't) as the codebase migrates onto the `vm` / `backend` / `backend/qemu` / `guest` /
`manifest` library API.

Dispositions used below:

- **Unchanged** — no consumer-visible difference.
- **Compatible rewire** — internals move onto the new API; behavior and
  formats are preserved.
- **Frozen, revisit later** — the format/protocol is kept byte-compatible
  through the migration; redesign is explicitly deferred.
- **Breaking, with transition** — consumers must adapt; a fallback or
  overlap period is provided.
- **New** — surface that does not exist today.

## 1. CLI commands and flags — Compatible rewire

Today (`main.go`): `virtle launch [--resume no|auto|force] [--ssh]
[remote-cmd...]`, `virtle suspend`, `virtle hotplug [--detach] <id>`,
`virtle rpc <method> [json-args]`, `virtle manifest
{defaults [--resolved], validate, resolve, schema}`, `-v/--verbose`,
`--manifest`, and error exit codes via `manager.ExitCode`.

Target: identical commands, flags, output, and exit codes. The CLI's
implementation moves from `internal/manager` onto the public API — it
becomes the first consumer:

- `launch` → `manifest.Load` → `Backend.Start` → `Instance` +
  `RemoteControl()` for guest setup, plus CLI-layer session handling (SSH,
  host `[run]` processes, notifications, control socket).
- `suspend` → assert `backend.Suspender`, call `Suspend`.
- `hotplug` → assert `backend.DeviceAttacher`.
- `manifest *` subcommands → `manifest` package (decode/defaults/validate/
  schema exactly as today).

**New:** `virtle guest` subcommand — runs the guest daemon
(`guest.Serve`) inside a VM. Additive; no existing command changes.

## 2. `go install` path — Unchanged

The repo root remains `package main`, so
`go install github.com/shazow/virtle@latest` keeps working. This was a
deciding factor for namespacing the library under subpackages (`vm`,
`backend`, ...) instead of claiming the root package.

## 3. Manifest TOML format and JSON schema — Compatible rewire

The manifest input format (`internal/manifest/document.go`, published
`manifest.schema.json`, `docs/getting-started/`, `examples/*.toml`) is a
consumer contract: existing manifests must keep loading with identical
defaults, validation, and semantics.

What changes is what the manifest *lowers to*. `manifest.Load` maps
sections onto the new API:

| Manifest section | Lowers to |
|---|---|
| cpu/memory, `[machine]`, kernel/initrd | `vm.Spec` (CPUs, Memory, Kernel) |
| `[[mounts]]` (virtiofs / 9p / image) | `vm.Spec.Shares` / `vm.Spec.Disks` |
| `[network]` forwards | `vm.Spec.Ports` |
| `[write_files]` | `vm.Spec.Files` |
| volumes / persistence / state dirs | `vm.Spec.Disks`, `vm.Spec.Dir` |
| `[qemu]` (exec, machine_options, seccomp, sockets, devices, passthrough args) | `qemu.Backend` fields |
| `[hotplug]` | CLI-held device set, applied via `backend.DeviceAttacher` |
| balloon device/controller config | `qemu.Backend` + `backend.MemoryResizer` policy loop in the CLI layer |
| `[run]` host helper commands | CLI layer (session orchestration, per D5) |
| `[ssh]`, autoprovision | CLI layer, over `Guest` file operations |
| `[notifications]` | CLI layer |

Consequences to verify during migration: `virtle manifest schema` output
stays semantically identical (the schema reflects the TOML input, which does
not change), and `virtle manifest resolve` output — which today prints the
resolved *internal* manifest — may change shape. The resolved form was
always documented as internal; we will keep the command but its output
follows the new lowering. Flagged as the one soft spot in an otherwise
compatible surface.

## 4. Control socket JSON-RPC (`virtle.sock`) — Frozen, revisit later

Today (`internal/manager/control`): methods `status`, `methods`, `suspend`,
`hotplug`, `balloon`, `guest-ps`, `guest-exec`, `guest-read`, `guest-write`;
JSON envelope with typed error codes (`unknown_method`, `invalid_params`,
...); consumed by `virtle rpc` and any external tooling scripted against the
socket.

Through the migration the wire format is kept byte-compatible — including
the QMP-named fields in `status` responses (`qmpSocket`, `guestAgentSocket`,
`qmpReadyAt` stats), which are compat-frozen even though the Go identifiers
behind them get neutral names. The server's implementation rewires: `guest-*`
methods dispatch over `vm.Guest` (so they work identically whether the VM
runs QGA or the virtle guest daemon), `suspend`/`hotplug`/`balloon` dispatch
over the `backend` capability interfaces.

Deferred redesign: once the guest daemon exists, `guest-*` over the host
control socket duplicates what `guest.Dial` offers directly. Whether the
control socket keeps proxying guest operations or narrows to host-session
concerns is a post-migration decision; nothing breaks either way in this
phase.

## 5. On-disk and runtime state — Breaking (backend-private), with transition

Current state on disk, all produced by `internal/manager/launch` +
`internal/manager`:

- Runtime dir (working-dir, XDG `$XDG_RUNTIME_DIR/agentspace/<hostname>/`,
  or explicit path) holding sockets: QMP, guest-agent, ssh-ready, control,
  virtiofs sockets.
- Suspend state JSON (`launch/state.go`: `hostName`, `qmpSocketPath`,
  `vmStatePath`, `cid`, `timestamp`, `status`) + QEMU migration state file.
- Runtime lock / PID files (`launch/runtime_lock.go`).
- Hotplug state JSON (`internal/hotplug`).
- Volume images created on demand (`launch/filesystem.go`).

Target: all of this becomes **`backend/qemu`-private implementation detail**
(except the control socket, which stays a CLI-layer contract, §4). The
suspend state dir is whatever `backend.Suspender.Suspend(ctx, inst,
stateDir)` writes; its format is owned by the backend and explicitly not a
public contract going forward.

Transition rules:

- Suspend state is *not* portable across the migration: a VM suspended by
  pre-migration virtle is not resumable by post-migration virtle. The
  suspend-state file gets a version marker so the failure is a clear error
  ("suspended with an older virtle; resume with that version or discard"),
  not a corrupt-state crash. Suspends are short-lived by nature; we accept
  this break rather than carrying a format shim.
- Lock files and socket paths keep their locations during the migration so
  concurrent old/new invocations still exclude each other; relocation, if
  any, comes after.
- Nothing else on disk is a promise today (documented nowhere), and this
  document makes that explicit for the future: only the manifest format and
  the control socket are contracts.

## 6. Guest image requirements — Breaking, with transition

Today a functional guest requires **qemu-guest-agent** listening on the
virtio-serial channel; guest exec, file read/write, `ps`, SSH key
autoprovision, and graceful shutdown all route through it
(`internal/qga`, `internal/manager/guest_*.go`).

Target: guests run the **virtle guest daemon** (`virtle guest`, package
`./guest`) over vsock or serial socket; QGA becomes optional and eventually
unsupported.

Transition (matches D7 in library-api.md):

1. `backend/qemu` ships both `vm.Guest` implementations: the guest-daemon client
   (preferred) and the QGA adapter (fallback, equivalent to today).
   `RemoteControl()` returns whichever the spec/backend config selects —
   default: daemon if configured into the guest, else QGA.
2. Guest images add the `virtle` binary (or a stripped `virtle-guest`) to
   their init; getting-started docs gain a section on this.
3. After a deprecation window, the QGA adapter is removed from `backend/qemu` and
   qemu-guest-agent disappears from image requirements.

Image builders are the affected consumers; until step 3 nothing breaks for
them.

## 7. Go library consumers — New (the point of the exercise)

There is no supported Go API today; issue #66's consumers import
`./internal` via forks. Migration mapping from the internals they reach for
to the supported API:

| Reached for today | Supported replacement |
|---|---|
| `internal/manifest` `DecodeDocumentBytes` → `doc.Manifest()` | `manifest.Load(r) (*vm.Spec, backend.Backend, error)` |
| `internal/manager` `LaunchWithOptions` | `backend.Start(ctx, spec)` + CLI-layer composition |
| `internal/manager/launch` `BuildPlan` / `AcquireCID` / `WaitForSockets` / `ProcessSet` | `backend/qemu` internals — no longer consumer-facing; the plan/CID/socket dance happens inside `Backend.Start` |
| `internal/manager/qemu.go` argv building | `qemu.Backend` fields; argv construction is private |
| `internal/executor` `Runner` / `Command` / `Group` | private to backends; consumers hold a `backend.Instance`, not a process |
| `internal/qga` client | `inst.RemoteControl()` → `vm.Guest` |
| `internal/qmpclient` (suspend/migration) | `backend.Suspender` |
| `internal/balloon` | `backend.MemoryResizer` |
| `internal/hotplug` | `backend.DeviceAttacher` |
| `internal/manager/control` `Dial` | unchanged host socket (§4); Go client promotion deferred |
| `internal/units` | `int64` bytes in `vm.Spec` (D6, pending) |

API stability posture: the module is untagged v0; promoted packages carry an
experimental notice until the API settles, then a `v0.x` tag makes versions
readable. No compatibility promise before v1.

## 8. Phasing summary

| Phase | What lands | Consumer impact |
|---|---|---|
| 1 | `vm` core types + `backend/qemu` backend wrapping existing internals; CLI rewired onto them | None intended (CLI/manifest/control byte-compatible); suspend-state version marker introduced |
| 2 | `manifest.Load` public; examples + README library section | New Go API available |
| 3 | `guest` daemon + client; `virtle guest` subcommand; `RemoteControl` prefers daemon | Additive; image builders may opt in |
| 4 | QGA fallback deprecated, then removed | Image builders must ship the guest daemon |
| 5 | Control-socket guest-proxying revisited; state relocation if desired | Scripted socket consumers, if any changes are chosen |

Each phase keeps `go build ./... && go test ./...` and `nix flake check`
green and is separately landable; phases 1–2 deliver issue #66.
