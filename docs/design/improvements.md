# Library API: improvements to the shipped surface

Status: living plan, written as an implementation handoff. Sibling to
[roadmap.md](roadmap.md) (new functionality) and [guest.md](guest.md) (the
guest daemon); refs [#66](https://github.com/shazow/virtle/issues/66),
[#67](https://github.com/shazow/virtle/pull/67).

This document specifies changes to the public API as it stands on `main`
at v0.4.0 plus [#94](https://github.com/shazow/virtle/pull/94) (the
Firecracker backend). Every item was traced against real call sites on
that tree; the trace is kept with each item because it is what justifies
the change and what an implementer should re-verify before starting.

## Shipped

The first version of this plan (phases A–G, in this PR's history) has
landed, so it is not repeated here. For the record, and so nobody
re-proposes it: `backend.Machine` with `Done`/`Err`/`Wait`/`Kill`/
`Shutdown` and live-object capabilities; exported zero-value `qemu.Backend`
and `qemu.Machine` with compile-time capability assertions; `Guest.Run`
with the `os/exec` contract (`*vm.ExitError`, `vm.Output`); one public
`units` package with text/JSON/TOML codecs; `qemu.Accel`, `vm.Proto`,
`TermOptions.TermType`, `CopyOptions.Chown`; `control.Dial` returning a
`backend.Machine` proxy with the `wait` RPC and `backend.StatusReporter`;
`backend/backendtest` and `vm/vmtest`; the backend-neutral
`internal/session` loop; the `backend.Shutdown` helper removed. Consumer
detail is in HISTORY.md under 2026-09-01 and 2026-09-09.

## How to use this document

Items are in landing order. Each is one PR unless stated otherwise, and
each has four parts: **Change**, **Why** (the trace), **Files**, and
**Done when**. Cleanups that belong to an item are listed inside it; the
closing section lists what was inspected and deliberately left alone.

Ground rules that apply to every item:

- Pre-v1, two backends in-tree: breaking the Go API is acceptable and
  expected. Two things are frozen and must stay byte-compatible: the
  manifest TOML/JSON format (and its generated schema) and the
  control-socket wire format, including its QMP-named JSON fields.
- Invariants from the roadmap hold: one-way imports (`backend` → `vm`,
  never the reverse), capabilities as standalone `-er` interfaces
  discovered by assertion (never added to the core interfaces), zero VMM
  vocabulary outside its backend package, sealed unions over `any`, and
  `errors.ErrUnsupported` for anything a backend cannot honor.
- Follow AGENTS.md: `io`/`fs` interfaces over paths, evergreen tests of
  affirmative behavior, no timed sleeps, commit messages as
  `<component>: <short description>`.
- Each PR keeps `go build ./... && go test ./... && nix flake check`
  green (the KVM checks need a `kvm` builder; see CONTRIBUTING.md),
  updates the examples and package docs to the new API, and adds a line
  to HISTORY.md for consumer-visible changes.
- Design references, when a shape question comes up: `net/http`'s server
  and `Shutdown`/`Close` split, `database/sql/driver`'s capability
  placement, `os/exec`'s `ExitError`, `x/net/nettest` and `testing/fstest`
  for conformance suites, `tsnet.Server` for zero-value-usable servers.

## 1. `virtle hotplug` goes through the `Machine` proxy

**Change.** The CLI's `hotplug` command stops marshalling a
`control.HotplugRequest` and calling `control.Raw`; it dials the proxy,
asserts `backend.DeviceAttacher`, and calls `Attach`/`Detach` with the
`vm.Device` the manifest declares under the given id. The `id` field of
the `hotplug` RPC stays on the wire for older servers and `virtle rpc`.

```go
// before: the only CLI command still built on the raw escape hatch
params, _ := json.Marshal(control.HotplugRequest{ID: id, Detach: detach})
_, err = control.Raw(ctx, socketPath, "hotplug", params)

// after: the same contract as suspend and status
m, err := control.Dial(ctx, socketPath)
a, ok := m.(backend.DeviceAttacher)
dev, err := loaded.HotplugDevice(id) // the manifest's [[hotplug]] entry as a vm.Device
err = a.Attach(ctx, dev)
```

**Why.** `main.go` performs `suspend` and `status` through
`control.Dial(...).(backend.Suspender)` and `.(backend.StatusReporter)`,
but `runHotplug` still goes through `Raw` with the manifest-declared
device *id*, because the proxy's `Attach`/`Detach` take a `vm.Device` and
lower it to `HotplugRequest.Device` (`internal/control/client.go`,
`deviceRequest`), while the server accepts either form
(`vmm/hotplug_feature.go` resolves `req.Device` through the manifest and
otherwise hands `req.ID` to `hotplug.Runner.Attach`, which looks it up in
the resolved manifest's `Hotplug []HotplugDevice` table). The missing
piece is on the CLI side: the inverse of the server's
`controlHotplugDevice`, turning a `[[hotplug]]` entry into the `vm.Device`
it declares. Once the CLI has that, the id-only wire form has no in-tree
client and `Raw` is used by `virtle rpc` alone, which is the intended
shape.

**Files.** `main.go` (`runHotplug`), `internal/manifest` (a lookup on the
resolved manifest from hotplug id to `vm.Device`),
`internal/control/client_test.go` (attach/detach over the proxy against a
fake server).

**Done when.** `main.go` calls `control.Raw` only from `runRPC`; a
manifest-declared device attaches and detaches through the proxy in a
test; the `hotplug` RPC still accepts `{"id": ...}` (existing transport
tests unchanged).

## 2. Manifest consolidation and the overlay contract

Roadmap item 2; lands as one PR or a short series.

**Change.**

1. Split the manifest *input contract* — document types, defaults,
   validation, the JSON schema — into a leaf package that imports neither
   backend, and fold the rest of `internal/manifest` into the public
   `manifest` package.
2. `manifest.Load` then returns a Spec and a backend that are complete:
   no document carried inside either backend, no overlay rule, and no
   `NewBackendFromDocument`. Manifest sections with no Spec
   representation (`[run]`, `[notifications]`, `[ssh]`, balloon and
   hotplug tables) become fields of the configured backend, set by the
   loader the way `main.go` sets `Logger` and `ConsoleOutput` today.
3. `manifest.LoadDocument` goes: the CLI decodes once through the public
   package like any other consumer.
4. `virtle manifest {defaults,validate,resolve,schema}` move onto the
   public package.

**Why.** The cycle is unchanged (machinery → `internal/manifest` →
would-be `manifest` → backends → machinery) and #94 doubled its cost:
`backend/qemu` and `backend/firecracker` each export a
`NewBackendFromDocument(doc imanifest.Document, b Backend)` bridge that
is documented as "not callable (and not supported) outside the module",
each keeps a `doc *imanifest.Document` field and overlays the Spec on it
at `Start` (positional replacement of shares, disks, forwards, and
files), and `manifest.LoadDocument` exists only so `main.go` can reuse
its decoded document for `ManifestWithOptions`. `manifest.Load` has one
in-tree consumer path (`main.go` → `LoadDocument`) plus `tests/e2e`,
which boots a root disk through it on both backends — so the overlay
semantics now have an end-to-end test, and consolidation must keep that
scenario passing.

**Files.** `manifest/*`, `internal/manifest/*` (moves), `backend/qemu/
qemu.go` and `backend/qemu/spec.go`, `backend/firecracker/firecracker.go`
and `backend/firecracker/spec.go`, `main.go`, `tests/e2e/e2e_test.go`
(the manifest-driven boot scenario must pass unchanged).

**Done when.** `NewBackendFromDocument`, `LoadDocument`, and both `doc`
fields are gone; the TOML input format and generated JSON schema are
byte-identical (existing manifest and schema tests pass unchanged);
`virtle manifest resolve` output (documented as internal) may change.

### Cleanups with item 2

- The bidirectional converter pairs collapse to one lowering direction
  per backend: `backend/qemu/spec.go` (Spec → Document, 345 lines) and
  `backend/firecracker/spec.go` (122 lines) against
  `manifest.specFromDocument` (Document → Spec).
- `backend/firecracker/manifest_test.go` and the `[firecracker]`
  resolution tests in `internal/manifest/firecracker_test.go` move with
  the code they exercise.

## 3. A `manifest.Loader` for library users

With item 2, or before it as a small independent change.

**Change.**

```go
// Loader configures the backends Load returns; the zero value is Load's
// current behavior (discarding logger, console output to os.Stderr).
type Loader struct {
	Logger        *slog.Logger
	ConsoleOutput io.Writer
}

func (l *Loader) Load(r io.Reader) (*vm.Spec, backend.Backend, error)
func Load(r io.Reader) (*vm.Spec, backend.Backend, error) // (&Loader{}).Load
```

**Why.** Both backends carry `Logger` and `ConsoleOutput` fields with
zero-value defaults, and `main.go` sets them after `LoadDocument` with a
type switch over `*qemu.Backend` and `*firecracker.Backend`. A library
user of `manifest.Load` gets the defaults and has to repeat that switch
to change them — which also means naming both backend packages, the one
thing `manifest.Load` exists to spare them. The `http.Client` /
`http.DefaultClient` shape (a struct with the knobs, a package function
for the zero value) fits; a variadic options function does not, because
the knobs are two plain fields. #94 lists this among its deliberate
follow-ups.

**Files.** `manifest/manifest.go`, `main.go` (drop the type switch),
`manifest/manifest_test.go`.

**Done when.** `main.go` configures logging and console output through
the loader and names neither backend package on the launch path.

## 4. A shared backend skeleton

Roadmap item 3; internal, no public API change.

**Change.** Hoist the parts both backends implement twice into an
internal helper they both call:

- the `<state_dir>/<host_name>.lock` VM-name lock with stale-socket
  replacement (`launch/runtime_lock.go` on QEMU; inline in
  `firecracker/lifecycle_linux.go`);
- control-server bring-up: `control.Listen` + `control.NewMachineRouter`
  + `control.NewServer` + drain-before-exit (`runtime/concrete.go` on
  QEMU; `lifecycle_linux.go` on Firecracker);
- the ephemeral state directory for an empty `Spec.Dir` (a `MkdirTemp`
  removed on exit, in `qemu.Backend.Start` and `firecracker.Backend.Start`);
- the console hub wiring (`console.New` in `vmm/startup.go` and in
  `lifecycle_linux.go`).

**Why.** With one backend these were guessed at; with two they are
visible as copies, and #94 names the first two as follow-ups. A third
backend (libkrun, roadmap) should start from the skeleton, not from a
copy of the Firecracker file. The helper stays internal — it is
machinery, and the public `backend` package must not grow implementation
scaffolding.

**Files.** New `internal/machinery` (or a better name found while
extracting), `backend/qemu/internal/{launch,runtime,vmm}`,
`backend/firecracker/lifecycle_linux.go`.

**Done when.** Each of the four concerns has one implementation;
`backendtest.TestBackend` still passes against both backends
(`go test -tags=integration`) and the e2e checks are green.

## 5. Session hooks demolition

Roadmap item 5; lands with the guest daemon (roadmap item 1). Not before.

**Change.** Delete `backend/qemu/session` and the code that exists only
for it, replacing each piece with a backend-neutral mechanism:

| Today (QEMU-only, in `backend/qemu/session`) | After the daemon |
|---|---|
| `Ready`: dial the `SSH-READY` readiness socket, bounded by `VIRTLE_SSH_READY_TIMEOUT` (`readiness.TimeoutFromEnv`) | the daemon's hello, on both backends; `session.Hooks.Ready` defaults to it |
| `RunSSH`: `launch.RunSSHSession` over the vsock destination with QGA key autoprovision (`launch.SSHAutoprovisionKey`, `installSSHKey` through `RemoteControl`) | `ssh` against the daemon's sshd through a `virtle`-provided ProxyCommand ([guest.md](guest.md)) |
| `SSHCommandHint`: renders the manifest's ssh command for the CID | the same hint rendered from the ProxyCommand form |
| `Start`: `launch.HasSavedSuspendState` selects resume for `ResumeAuto` | stays, moved next to `internal/sessionbridge`, which already owns CLI-side suspend persistence |

**Why.** `backend/qemu/session` is 229 lines whose only export is
`Hooks()`, wired by a type switch in `main.go`. Everything it does is
QGA-era: the readiness token is written by the guest's SSH setup, the key
autoprovision installs through `RemoteControl` (QGA), and the SSH attach
targets `vsock/<cid>` directly, which does not exist on Firecracker
(guest.md's transport table). `internal/readiness` and `internal/sshtools`
have no importers outside this package and `launch/ssh*.go`; they go with
it. `TimeoutFromEnv` — configuration by environment variable inside
library code — must not be ported.

**Files.** Delete `backend/qemu/session`, `internal/readiness`,
`internal/sshtools`, `backend/qemu/internal/launch/{ssh.go,
ssh_session.go}` and their tests; `main.go` (drop the hooks switch);
`internal/session` (neutral `Ready` default; SSH attach over the
ProxyCommand).

**Done when.** `session.Hooks` is gone or has no per-backend
implementation; `--ssh` works on both backends against the same
`internal/session` code path; no `VIRTLE_SSH_READY_TIMEOUT`.

### Cleanups with item 5

- The last SSH-era test in `vmm/manager_test.go`,
  `TestBuildQEMUCommandOmitsSSHReadyDeviceWhenSocketEmpty`, goes with the
  readiness socket device.
- `backend/qemu/session/session_test.go` goes with its package; the
  neutral loop is already tested in `internal/session/session_test.go`
  against `backendtest.NewMemoryBackend`.

## 6. QGA removal cleanups

With roadmap item 6, after the overlap window.

- `vmm.VM.DialGuestAgent` and `ShutdownGuest` (`vmm/api.go`) have exactly
  one caller each, the QGA adapter in `backend/qemu/guest.go`; they leave
  with it, along with `backend/qemu/internal/qga` and `launch/qga.go`
  (`WaitForGuestAgent`).
- `qemu.Backend.RemoteControl`'s union keeps only the daemon member; the
  `hasRemoteControl` gate on guest RPCs stays.
- The `guest-*` control-socket methods stay on the wire (frozen) but are
  served by the daemon client rather than QGA.

## 7. Test layout cleanups

Independent of everything above; one PR, behavior-neutral.

- `vmm/manager_test.go` is 3,842 lines. Move the 15 `TestBuildQEMUCommand*`
  tests (plus the one in `balloon_test.go`) into a `qemu_test.go` beside
  `vmm/qemu.go`, the file whose argument lowering they test. Move the two
  `TestCreateVolumeImage*` tests beside `launch.CreateVolumeImage`
  (`launch/filesystem.go`), the function they call; what they check about
  `chattr` and sizing now lives in `internal/diskimage`, so decide there
  which assertions belong to `diskimage`'s own tests.
- Consolidate the fakes defined twice across `vmm/manager_test.go` and the
  `launch`, `balloon`, and `hotplug` test files: `fakeQMPDialer`,
  `fakeQMPClient`, `fakeNotifier`, `fakeMonitor`, `fakeGuestAgentDialer`,
  `fakeGuestAgentClient`. One doubles package per seam, as
  `internal/executor/executortest` already does for the process seam.
  The guest-agent pair goes with item 6 instead if that lands first.
- `TestManagerStartQEMUNilRunnerWrapsOnce` names a fixed defect rather
  than a behavior; fold it into the affirmative start-failure test.

## 8. Smaller follow-ups from #94

Each is independent and small; batch them as convenient.

- **Derived duration fields in Firecracker's `Status`.** Firecracker's
  machine records `StartedAt`, `MonitorReadyAt`, and `CompletedAt` in
  `RuntimeStats` but never the derived strings QEMU fills
  (`StartedToBoot`, `BootToMonitor`, `Total`, ...), so `virtle status`
  reads differently on the two backends. Compute them at the same
  transitions. No wire change: the fields exist.
- **Fold `docs/recipes/firecracker/check.py` into the e2e runner.** The
  `firecracker` flake check drives the Firecracker recipe through its
  own Python script while `tests/e2e/run.py` boots both backends; one
  runner with a recipe fixture removes the second script.
- **A Spec-level readiness hook** so `Start` callers need not scan the
  console themselves. Superseded for daemon guests by the hello
  (roadmap item 1); still worth having for agentless appliances as a
  console marker line — decide its shape only after the daemon's
  readiness lands, so the two do not compete.
- **`firecracker.Backend.Shutdown` on aarch64** returns
  `errors.ErrUnsupported` after killing the VMM (no `SendCtrlAltDel`);
  the fix is daemon-driven shutdown (roadmap item 1), not a backend
  change.

## Inspected and deliberately kept

So nobody re-litigates them:

- `internal/executor/executortest`: test-only by design; many test files
  use it.
- `control.Raw` and `virtle rpc`: the intended debugging escape hatch
  (`kubectl get --raw`, `virsh qemu-monitor-command` precedent). Item 1
  removes its last non-`rpc` caller; it does not remove `Raw`.
- `internal/session.ExitCode`: the CLI's exit-status mapping for the
  shared loop, not a leftover delegate.
- `internal/sessionbridge`: keeps CLI-only suspend persistence out of the
  backend API on purpose (#94).
- The `id` field of the `hotplug` RPC: frozen wire format, kept for
  older clients even once the CLI sends `device`.
- `manifest.Load`'s positional overlay semantics: a transitional contract
  until item 2, now covered end to end by `tests/e2e`; do not extend it.
- `commandArguments` (`balloon/qmp_test.go`) and `commandEnvAdditions`
  (`launch/ssh_test.go`): test helpers with callers, not dead code.

## Sequence summary

| Order | Item | Ships with |
|---|---|---|
| 1 | `virtle hotplug` over the proxy | one PR, now |
| 2 | Test layout cleanups (item 7) | one PR, any time |
| 3 | `manifest.Loader` (item 3) | one PR; independent of item 2 |
| 4 | Manifest consolidation (item 2) + cleanups | roadmap item 2 |
| 5 | Shared backend skeleton (item 4) | roadmap item 3; before a third backend |
| 6 | Session hooks demolition (item 5) + cleanups | roadmap item 1 (the daemon) |
| 7 | QGA removal cleanups (item 6) | roadmap item 6 |
| — | Smaller follow-ups (item 8) | as convenient |

Every step keeps `go build ./... && go test ./... && nix flake check`
green and is separately landable.
