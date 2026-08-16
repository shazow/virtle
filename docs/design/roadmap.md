# Library API: what remains

Status: living document (refs [#66](https://github.com/shazow/virtle/issues/66)).
This supersedes the earlier `library-api.md` and `migration.md` design docs:
the API they specified is implemented, so the code is now the reference —
`vm`, `backend`, `backend/qemu`, `manifest`, `units` on
[pkg.go.dev](https://pkg.go.dev/github.com/shazow/virtle) — and this document
records only the architectural invariants worth preserving and the work that
remains. The removed docs (and PRs #67, #71, #72, #74, #76) hold the full
rationale in git history.

## Where things stand

The public library API exists and the CLI is its first consumer:

- `vm` — consumer-facing: `Spec` (neutral, constructable in Go), `Guest`
  (os/exec- and io/fs-flavored guest operations), `Term`, the sealed
  `Device` union (`Share`, `Disk`, `Forward`), `GuestWithCopy` +
  `ArchiveFS` (streamed-tar file trees).
- `backend` — implementer contract: `Backend`, `Instance`, and standalone
  capability interfaces discovered by type assertion (`Suspender` with
  `StateVersion()`, `MemoryResizer`, `DeviceAttacher`, `ConsoleProvider`),
  plus the `Shutdown` helper. Absent capabilities return
  `errors.ErrUnsupported`.
- `backend/qemu` — the QEMU backend: `qemu.New(qemu.Config{...})`, with
  guest control selected by the sealed `RemoteControl` union (`qemu.QGA{}`
  today; `qemu.Guest{}` reserved for the native daemon; nil = agentless).
- `manifest` — `Load(r) (*vm.Spec, backend.Backend, error)`.
- `units` — `units.Bytes` + IEC constants, `units.Duration`.
- The CLI (`main.go`) drives launches through `backend/qemu/session`, a
  deliberately minimal transitional facade (`Run`, `Suspend`, `Hotplug`,
  `ExitCode`, logger hooks); `suspend`/`hotplug`/`rpc` remain
  control-socket clients of a running session, by design.

## Invariants to preserve

These are the load-bearing decisions; changes to them are design changes,
not refactors.

- **`database/sql`-shaped split.** Consumer package `vm`, contract package
  `backend`, implementations under `backend/`. Import direction is one-way
  (`backend` imports `vm`, never the reverse), which structurally forbids a
  default backend — consumers always name one (`qemu.New`).
- **Capabilities are discovered, not designed.** Core interfaces stay
  minimal; optional functionality is standalone package-qualified `-er`
  interfaces asserted on the backend, never added to `Backend`/`Instance`.
- **Zero QEMU vocabulary outside `backend/qemu`**, compiler-enforced: all
  qemu machinery lives under `backend/qemu/internal/` (`vmm`, `launch`,
  `runtime`, `qmpwire`, `qmpclient`, `qga`, `balloon`, `hotplug`). Shared
  `internal/` holds only backend-agnostic code (`control`, `executor`,
  `manifest`, `sshtools`, `readiness`, `units`). A future backend
  (firecracker, libkrun) implements the `backend` contract against these —
  it does not reuse qemu's machinery. Hoisting a shared orchestration
  skeleton waits until a second backend shows which parts are common.
- **The manifest owns its resolved vocabulary.** Resolved device types
  (`manifest.BalloonDevice`, `manifest.HotplugDevice`, …) live in the
  manifest package; backends consume them, not the other way around.
- **Sealed unions over `any`** for closed type sets (`vm.Device`,
  `qemu.RemoteControl`), so adding members is additive and switches stay
  typed.
- **Only two compatibility contracts:** the manifest TOML format (and its
  JSON schema) and the control-socket wire format (`internal/control`,
  byte-compatible including its QMP-named compat-frozen fields).
  Everything else on disk — suspend state, sockets, locks — is
  backend-private; suspend state is stamped with the backend-owned version
  token (`Suspender.StateVersion()`, `"qemu-v1"`) and only an exact match
  resumes.
- **Session architecture keeps the driver shape**: the CLI owns the
  foreground loop, because the interactive attach hands the terminal to a
  child process and signals are process-global. Lifecycle hooks stay
  observation-only (the notification sink); control flow is never inverted
  into callbacks.

## Remaining work

In rough dependency order. Each step keeps
`go build ./... && go test ./... && nix flake check` green and lands
separately.

### 1. Manifest consolidation

`internal/manifest` (document decode, defaults, validation, resolution,
schema) is still shared-internal; the public `manifest` package wraps it
via the module-internal `qemu.NewBackendFromDocument` bridge. Folding it
into public `manifest` closes an import cycle today: machinery →
`internal/manifest` → would-be `manifest` → `backend/qemu` → machinery.

Plan: split the *input contract* (document types, defaults, validation —
what the machinery actually consumes) into a leaf package with no backend
dependency, so public `manifest.Load` can keep returning a configured
backend without the cycle. `qemu.NewBackendFromDocument` disappears; the
`virtle manifest {defaults,validate,resolve,schema}` subcommands move onto
the public package. Note `virtle manifest resolve` prints the resolved
internal form — documented as internal, its output may change shape with
the lowering; the TOML input format and schema must not change.

### 2. The virtle guest daemon (design "D9")

The largest remaining piece: a `guest` package with `Serve` (runs in-guest
over vsock/unix; the `virtle guest` CLI subcommand wraps it) and `Dial`
(host side; the returned client implements `vm.Guest`), replacing QGA.

Requirements settled in review (PR #67):

- **Protocol**: binary-safe (no base64 — the QGA limitation this
  replaces), concurrent (multiple requests in flight per connection),
  streaming (bulk data as streams, not chunked rounds), and it opens with
  a **version handshake** — the daemon is baked into guest images and will
  routinely skew against the host virtle, so mismatch fails or degrades
  explicitly, never silently.
- **Terminal/SSH**: the daemon embeds a rudimentary sshd (`x/crypto/ssh`,
  Tailscale-SSH shape: exec/shell/pty channels only), vsock-constrained
  with publickey auth via a host-provisioned key (vsock loopback means
  unprivileged guest processes can reach the port). SSH serves humans and
  their tooling; the typed virtle RPC remains the programmatic transport.
  Candidate framing: reuse SSH's channel mux for RPC and streams alike —
  one mux instead of two. `vm.Term` + `backend.ConsoleProvider` already
  cover the no-daemon serial-console debug path.
- **File trees**: implement `vm.GuestWithCopy` (streamed tar, zip-slip
  extraction guard enforced guest-side as an invariant, `CopyOptions`
  ownership overrides).
- **Portability**: the daemon ships inside the regular `virtle` binary;
  release builds are `CGO_ENABLED=0` static, enforced by a CI check.
  `./guest`'s dependency budget: stdlib + `golang.org/x/sys` +
  `x/crypto/ssh` only. A minimal `cmd/virtle-guest` main (~3 MB) can come
  later without build tags.
- **Backend wiring**: add the `qemu.Guest{Port}` transport to the
  `RemoteControl` union. The machinery seams are already in place —
  `vmm.GuestReadiness` (session readiness gate; QGA-era impl waits on the
  ssh-ready socket, the daemon swaps in its handshake) and
  `vmm.Config.GuestAgentDialer` (guest-control dial seam). Guest images
  add the binary to their init; getting-started docs gain a section.

### 3. Session-layer demolition

`backend/qemu/session` is QGA-era scaffolding with a demolition date, and
says so in its package doc. Once the daemon lands: the ssh-ready gate
becomes the daemon handshake, autoprovision becomes a host-pushed key at
boot, and the residual foreground loop is rebuilt over the *public*
backend API using an event-channel shape (machinery emits events, the CLI
selects) — which also dissolves the saved-suspend sentinel error into a
command/event pair. The driver-shape invariant above still holds; only
the plumbing under it changes.

### 4. QGA deprecation and removal

After a deprecation window with both transports shipping: remove the QGA
adapter from `backend/qemu` and qemu-guest-agent from the guest-image
requirements. This is the only breaking change for image builders, and
step 2 gives them the overlap period.

### 5. Post-daemon revisits

Explicitly deferred, none blocking:

- The control socket's `guest-*` methods duplicate what `guest.Dial`
  offers; decide whether the socket keeps proxying guest operations or
  narrows to host-session concerns.
- Promotion of a public Go client for the control socket.
- On-disk state relocation, if any (lock and socket paths deliberately
  stayed put through the migration so concurrent old/new invocations
  exclude each other).
- Hoisting a shared session/orchestration skeleton — only when a second
  backend exists to show the real seams.

### 6. Versioning

The module is untagged v0. Once the API settles (post manifest
consolidation is a reasonable point), start tagging `v0.x` so versions are
readable. No compatibility promise before v1.
