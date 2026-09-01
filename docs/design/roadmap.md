# Library API: remaining work

Status: living document (refs
[#66](https://github.com/shazow/virtle/issues/66),
[#67](https://github.com/shazow/virtle/pull/67)).

The library API designed in #67 is implemented and shipped (v0.3.x): the
code is the reference — `vm`, `backend`, `backend/qemu`, `manifest`,
`units`, with the CLI as the first consumer. This document records only
what remains, written against the API *as built* — which deviated from the
original design docs in a few places that shape the remaining work. The
full design rationale lives in #67's review threads and git history.
Planned changes to the *shipped* surface are tracked separately in
[improvements.md](improvements.md); the guest daemon's design is in
[guest.md](guest.md).

## As-built context that the remaining work builds on

Where implementation deviated from (or went beyond) the original design:

- **Guest-control selection is a sealed union, not constructors.** The doc
  specified `qemu.BackendWithQGA(...)` / `qemu.BackendWithGuest(...)`;
  what shipped is better: `qemu.New(Config)` with
  `Config.RemoteControl` holding a sealed `qemu.RemoteControl` union —
  `qemu.QGA{}` today, `qemu.Guest{}` reserved for the native daemon,
  nil = agentless. Daemon wiring is therefore *adding a union member*,
  not adding a constructor.
- **Suspend-state versioning landed as a capability method**:
  `backend.Suspender.StateVersion()` (`"qemu-v1"`); only an exact match
  resumes. The migration doc's "version marker" is done.
- **The session layer survived as scaffolding with a demolition date.**
  `backend/qemu/session` says so in its own package doc: it is QGA-era
  plumbing (ssh-ready gate, SSH attach loop with autoprovision,
  suspend-on-signal) that the guest daemon deletes. It is deliberately
  not a designed API.
- **QEMU machinery is compiler-quarantined** under
  `backend/qemu/internal/` (`vmm`, `launch`, `runtime`, `qmpwire`,
  `qmpclient`, `qga`, `balloon`, `hotplug`); shared `internal/` holds only
  backend-agnostic code. The daemon-facing seams already exist:
  `vmm.Config.GuestReadiness` (session readiness gate) and
  `vmm.Config.GuestAgentDialer` (guest-control dial).
- **Public `manifest` is a bridge, not yet the owner.** `manifest.Load`
  wraps `internal/manifest` via the module-internal
  `qemu.NewBackendFromDocument`; the resolved device vocabulary
  (`HotplugDevice`, balloon config, ...) still lives in
  `internal/manifest`.
- **Already done, dropped from this roadmap**: static builds
  (`CGO_ENABLED=0` enforced in release.yml and the flake) and version
  tagging (v0.3.x underway).

Invariants worth preserving (changes here are design changes, not
refactors): the `database/sql`-shaped split with one-way imports
(`backend` → `vm`, never the reverse; no default backend); capabilities as
standalone `-er` interfaces discovered by assertion, never added to
`Backend`/`Instance`; zero QEMU vocabulary outside `backend/qemu`; sealed
unions over `any` for closed type sets (`vm.Device`,
`qemu.RemoteControl`); exactly two compatibility contracts (manifest TOML
+ schema, control-socket wire format) with everything else on disk
backend-private; and the CLI keeping the driver shape (it owns the
foreground loop; lifecycle hooks are observation-only).

## Remaining work

In dependency order. Each step lands separately and keeps
`go build ./... && go test ./... && nix flake check` green.

### 1. Manifest consolidation

Fold `internal/manifest` (document decode, defaults, validation,
resolution, schema) into the public `manifest` package. Blocked today by a
cycle: machinery → `internal/manifest` → would-be `manifest` →
`backend/qemu` → machinery. Plan: split the *input contract* (document
types, defaults, validation — what the machinery consumes) into a leaf
package with no backend dependency, so `manifest.Load` keeps returning a
configured `backend.Backend` without the cycle. `qemu.NewBackendFromDocument`
disappears; the `virtle manifest {defaults,validate,resolve,schema}`
subcommands move onto the public package; the resolved device vocabulary
becomes public with it. `virtle manifest resolve` output may change shape
(documented as internal); the TOML input format and JSON schema must not.

### 2. The virtle guest daemon

The largest remaining piece: a `guest` package with `Serve` (in-guest,
over vsock/unix; the `virtle guest` CLI subcommand wraps it) and `Dial`
(host-side client implementing `vm.Guest`), replacing QGA. Detailed
design: [guest.md](guest.md). Requirements settled in #67 review:

- **Protocol**: binary-safe (no base64 — the QGA limitation this
  replaces), concurrent (multiple in-flight requests per connection),
  streaming (bulk data as streams), opening with a **version handshake** —
  the daemon is baked into guest images and will routinely skew against
  the host virtle, so mismatch fails or degrades explicitly, never
  silently.
- **Terminal/SSH**: the daemon embeds a rudimentary sshd
  (`x/crypto/ssh`, Tailscale-SSH shape: exec/shell/pty channels only;
  no sftp/forwarding initially), vsock-constrained with publickey auth
  via a host-provisioned key (vsock loopback means unprivileged guest
  processes can reach the root daemon's port). SSH serves humans and
  their tooling (scp, rsync, VS Code Remote-SSH) with zero image
  requirements; the typed virtle RPC remains the programmatic transport.
  Candidate framing: reuse SSH's channel mux for RPC and streams alike —
  one mux instead of two.
- **File trees**: implement `vm.GuestWithCopy` (the interface and
  `vm.ArchiveFS` already shipped; no implementation exists — QGA cannot
  stream). Streamed tar both directions; zip-slip extraction guard
  enforced guest-side as an invariant; `CopyOptions` overwrite and
  ownership semantics as specced.
- **Wiring**: add the `qemu.Guest{Port}` member to the `RemoteControl`
  union; swap `vmm.Config.GuestReadiness` from the ssh-ready-socket gate
  to the daemon handshake; the `GuestAgentDialer` seam takes the daemon
  client. Dependency budget for `./guest`: stdlib + `x/sys` +
  `x/crypto/ssh` (static enforcement already in CI). Guest images add the
  binary to their init; getting-started docs gain a section. A minimal
  `cmd/virtle-guest` main (~3 MB) can come later without build tags.

### 3. Serial console capability

`backend.ConsoleProvider` and `vm.Term` shipped as types, but no backend
implements them: wire the QEMU chardev/serial console up as the
no-daemon, no-agent debug path of last resort. Small, independent of the
daemon, and makes the capability story honest before QGA removal narrows
the alternatives.

### 4. Session-layer demolition

Once the daemon lands, retire `backend/qemu/session` per its own package
doc: the ssh-ready gate becomes the daemon handshake, autoprovision
becomes a host-pushed key at boot, and the residual foreground loop is
rebuilt over the *public* backend API with an event-channel shape
(machinery emits events, the CLI selects) — dissolving the saved-suspend
sentinel error into a command/event pair. The driver-shape invariant
holds; only the plumbing beneath it changes.

### 5. QGA deprecation and removal

After an overlap window with both transports shipping: remove the QGA
member from the `RemoteControl` union, delete
`backend/qemu/internal/qga`, and drop qemu-guest-agent from guest-image
requirements. The only breaking change for image builders; step 2 gives
them the overlap period.

### 6. Post-daemon revisits

Explicitly deferred, none blocking:

- The control socket's `guest-*` methods duplicate what `guest.Dial`
  offers; decide whether the socket keeps proxying guest operations or
  narrows to host-session concerns.
- Promotion of a public Go client for the control socket.
- On-disk state relocation, if any (lock and socket paths deliberately
  stayed put so concurrent old/new invocations exclude each other).
- Hoisting a shared session/orchestration skeleton — only when a second
  backend (firecracker, libkrun) exists to show the real seams.
- A v1 compatibility promise — not before manifest consolidation and the
  daemon settle the surface.
