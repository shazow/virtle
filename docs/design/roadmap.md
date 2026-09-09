# Library API: remaining work

Status: living document (refs
[#66](https://github.com/shazow/virtle/issues/66),
[#67](https://github.com/shazow/virtle/pull/67)). Written against `main`
at v0.4.0 plus [#94](https://github.com/shazow/virtle/pull/94) (the
Firecracker backend); the code is the reference and this document records
only what remains. Planned changes to the shipped surface are in
[improvements.md](improvements.md); the guest daemon's design is in
[guest.md](guest.md).

## Where things stand

The library API exists, the CLI runs on it, and there are two backends:

- `vm` — `Spec` (with the `Dir` contract: empty means the process working
  directory and ephemeral state; `Suspend`/`Resume` need a `Dir`),
  `Disk{GuestPath: "/"}` naming the root device on both backends, typed
  `Proto`, `Guest` with the `os/exec` contract (`Run` streams to writers,
  non-zero exit is `*vm.ExitError`, `vm.Output` buffers), `GuestWithCopy`
  + `ArchiveFS` + `CopyOptions` (declared; no implementation yet), `Term`
  + `TermOptions` + `ErrTermFellBehind`.
- `backend` — `Machine` (`Done`/`Err`/`Wait`/`Kill`/`Shutdown`/
  `RemoteControl`), `Status`/`State`/`StatusReporter`, and the standalone
  capabilities `Suspender`, `Resumer` (`StateVersion`), `MemoryResizer`,
  `DeviceAttacher`, `ConsoleProvider`. Absent capabilities are
  `errors.ErrUnsupported`.
- `backend/qemu` — exported zero-value `Backend` and `Machine` with
  compile-time capability assertions; `Accel`, `Console`, `DisableVSock`;
  guest control selected by the sealed `RemoteControl` union, which today
  has one member, `qemu.QGA{}` (nil = agentless).
- `backend/firecracker` (#94) — `Backend` (`Binary`, timeouts, `Console`,
  `HostName`, `Logger`, `ConsoleOutput`); machines implement
  `StatusReporter` and `ConsoleProvider`. Direct kernel boot, raw disks
  (created from `Disk.Size`), serial console as output and as a `Term`.
  Everything needing a guest transport (`RemoteControl`, `Files`, `Shares`,
  `Ports`), plus networking, suspend, balloon, and hotplug, fails with
  `errors.ErrUnsupported`. Linux + KVM only; guest arch = host arch.
- The console is real on both backends: `internal/console` is a hub
  between the VMM's stdio and `ConsoleOutput`, retains 256 KiB, and serves
  `Term`s that replay history then stream; slow readers are dropped with
  `ErrTermFellBehind`. Readiness without a daemon is a `bufio.Scanner`
  over the `Term`, which `tests/e2e` does on both backends.
- The control socket serves the `Machine` contract: `control.Dial(ctx,
  path)` returns a `backend.Machine` proxy (`Done`/`Err` via the additive
  `wait` RPC, `ErrUnsupported` for methods the server's `methods` list
  lacks), `control.NewMachineRouter` serves any `Machine`, `control.Raw`
  stays the `virtle rpc` escape hatch. `virtle status` and `virtle
  suspend` go through the proxy; `virtle hotplug` still uses `Raw` for its
  manifest-declared device id (see improvements.md).
- The CLI foreground loop is backend-neutral: `internal/session.Run(ctx,
  b, spec, mf, opts)` drives `backend.Machine` (start/resume by capability,
  a `Ready` hook, SSH attach, signals and control requests into an orderly
  suspend or shutdown), with backend-specific behavior entering only
  through `session.Hooks`. `backend/qemu/session` supplies the QEMU hooks:
  saved-state resume, the guest's `SSH-READY` token gate, and SSH attach
  with QGA key autoprovision. `internal/sessionbridge` keeps foreground
  suspend persistence CLI-only. `main.go` loads the manifest once and
  goes through `manifest.LoadDocument`.
- `manifest.Load`/`LoadDocument` return a Spec plus a configured backend
  for either VMM (`backend = "qemu" | "firecracker"`). Both backends still
  carry a loaded document and overlay the Spec on it at `Start`, through a
  per-backend `NewBackendFromDocument` bridge over `internal/manifest`.
- `units` is the single public home for unit logic (`Bytes`, `MiB`,
  `Duration`, `ParseBytes`, text/JSON/TOML codecs, `JSONSchemaTypes`).
- Tests: `backend/backendtest.TestBackend` is the conformance suite, run
  untagged against `backendtest.NewMemoryBackend` (over `vm/vmtest.Guest`)
  and, behind `//go:build integration`, against QEMU and Firecracker;
  `tests/e2e` boots both real VMMs under KVM through the CLI (`e2e-fast`)
  and the Go API (`e2e-api`) on a shared tinyconfig kernel + static
  BusyBox guest (`tests/e2e/fixtures/fast`), with `nix run
  .#benchmark-backends` comparing them.

Invariants to preserve (changes here are design changes, not refactors):
the `database/sql`-shaped split with one-way imports (`backend` → `vm`,
never the reverse; no default backend); capabilities as standalone `-er`
interfaces discovered by assertion, never added to `Backend`/`Machine`;
zero VMM vocabulary outside its backend package (`backend/qemu/internal/*`
holds all QEMU machinery; `backend/firecracker` has none of it); sealed
unions over `any` (`vm.Device`, `qemu.RemoteControl`); exactly two
compatibility contracts (manifest TOML + schema, control-socket wire
format, whose QMP-named JSON fields stay frozen behind neutral Go names)
with everything else on disk backend-private (`StateVersion` gates
resume); the CLI keeping the driver shape (it owns the foreground loop;
lifecycle hooks are observation-only); a backend refuses what it cannot
honor with `errors.ErrUnsupported` rather than dropping it.

## Remaining work

In dependency order. Each step lands separately and keeps
`go build ./... && go test ./... && nix flake check` green.

### 1. The virtle guest daemon

The gating item for everything guest-shaped, and now doubly so: it is the
only route to guest control on Firecracker, where there is no QGA and no
virtio-serial channel. Detailed design: [guest.md](guest.md), revised for
two backends. In brief: a `guest` package (`Server`, `Dialer`, `Client`
implementing `vm.Guest` and `vm.GuestWithCopy`), one SSH server over
vsock as the transport (session channels for humans, a `virtle` subsystem
for the typed RPC with a version-handshake hello, per-operation stream
channels), vsock peer-CID gating plus a host-provisioned key delivered on
the kernel command line, and an embedded sshd so scp/rsync/VS Code work
against bare images. Backend wiring is one union member per backend
(`qemu.Guest{}`, and Firecracker's vsock device configured through its
API) plus a backend-neutral readiness rule: the daemon's hello *is*
readiness, replacing QEMU's `SSH-READY` token hook and Firecracker's
console scanning. The daemon also gives Firecracker a real `Shutdown`
(ask the guest to reboot) in place of `SendCtrlAltDel`, which needs
i8042 in the guest and does not exist on aarch64. guest.md's last section
covers how the daemon gets *into* minimal guests.

### 2. Manifest consolidation

Fold `internal/manifest` (document decode, defaults, validation,
resolution, schema) into the public `manifest` package. The cycle is
unchanged (machinery → `internal/manifest` → would-be `manifest` →
backends → machinery) and the cost has doubled: two
`NewBackendFromDocument` bridges, two document overlays, and
`manifest.LoadDocument` exposed only to give the CLI a single load. Plan:
split the *input contract* (document types, defaults, validation) into a
leaf package with no backend dependency, so `manifest.Load` returns a
Spec and a backend that are complete, with no document smuggled inside
either backend. `virtle manifest {defaults,validate,resolve,schema}` move
onto the public package. The TOML input format and JSON schema must not
change; `virtle manifest resolve` output (documented as internal) may.

### 3. A shared backend skeleton

With two backends, the common parts are visible instead of guessed (#94
lists them as follow-ups): the `<state_dir>/<host_name>.lock` VM-name
lock with stale-socket replacement, control-server bring-up
(`control.NewMachineRouter` + listener + drain-before-exit), ephemeral
state-directory handling for an empty `Spec.Dir`, and the console hub
wiring. Hoist them into an internal helper both backends call, so a third
backend (libkrun) starts from the skeleton rather than from a copy.

### 4. Firecracker parity

Each item is a capability or a Spec feature that Firecracker currently
refuses with `ErrUnsupported`; none changes the public API.

- **Guest control, `Files`, and readiness** — item 1, over Firecracker's
  vsock device.
- **`Shares`** — Firecracker has no virtiofs or 9p. The daemon's
  `CopyToGuest`/`CopyFromGuest` is the file path on Firecracker; a
  `Share` stays `ErrUnsupported` there, documented as a backend difference
  rather than papered over.
- **Networking and `Ports`** — a TAP device configured through the API,
  with host-side forwarding done by the CLI/session layer as QEMU's user
  networking does today; needs a design note before implementation.
- **Suspend/Resume** — Firecracker snapshots (pause, snapshot, restore)
  can implement `Suspender`/`Resumer` with a Firecracker `StateVersion`;
  the CLI's suspend persistence already flows through `sessionbridge`.
- **`MemoryResizer`** — Firecracker's balloon device.
- **`DeviceAttacher`** — stays `ErrUnsupported`: Firecracker configures
  devices before boot only.
- **aarch64 `Shutdown`** — resolved by item 1.

### 5. Session hooks demolition

`backend/qemu/session` is the QGA-era remainder of the old session layer:
the `SSH-READY` token readiness probe (with the `VIRTLE_SSH_READY_TIMEOUT`
knob), SSH attach over the vsock destination, and QGA key autoprovision,
plus `readiness` and `sshtools` in shared `internal/`. Once item 1 lands:
readiness is the daemon hello for both backends (a neutral `Ready`
default, no per-backend hook), the key travels on the kernel command line
(no autoprovision), and `--ssh` attaches to the daemon's sshd through a
`virtle`-provided ProxyCommand that knows each backend's dial mechanism
(see guest.md). What remains of the QEMU hooks is saved-state resume
selection, which `sessionbridge` already carries.

### 6. QGA deprecation and removal

After an overlap window with both transports shipping: remove `qemu.QGA`
from the `RemoteControl` union, delete `backend/qemu/internal/qga` and the
QGA guest adapter, and drop qemu-guest-agent from guest-image
requirements. The only breaking change for image builders; item 1 gives
them the overlap period.

### 7. Deferred, none blocking

- A `manifest.Loader` carrying `Logger` and console output for library
  users of `Load` (today they get the zero-value backend's defaults).
- A Spec-level readiness hook so `Start` callers need not scan the console
  — superseded by item 1's hello for daemon guests; still useful for
  agentless appliances (a marker line on the console).
- Derived duration fields in Firecracker's `Status`.
- Folding `docs/recipes/firecracker/check.py` (the `firecracker` flake
  check's own runner) into the `tests/e2e` runner.
- Promotion of `control.Dial` as a documented public client, and whether
  the socket keeps proxying `guest-*` once `RemoteControl` on the proxy
  can hand off to the daemon directly.
- On-disk state relocation, if any.
- A v1 compatibility promise — not before items 1–3 settle the surface.
