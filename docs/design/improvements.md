# Library API: improvements to the shipped surface

Status: approved plan, ready to implement. Sibling to
[roadmap.md](roadmap.md) (new functionality) and [guest.md](guest.md)
(the guest daemon); refs [#66](https://github.com/shazow/virtle/issues/66),
[#67](https://github.com/shazow/virtle/pull/67).

This document specifies changes to the public API that shipped in v0.3.x
(`vm`, `backend`, `backend/qemu`, `manifest`, `units`). Every item was
traced against real call sites before it was accepted; the trace is kept
with each item because it is what justifies the change and what an
implementer should re-verify before starting.

## How to use this document

Items are grouped into phases in landing order. Each phase is one PR
unless stated otherwise, and each item has four parts: **Change**, **Why**
(the trace), **Files**, and **Done when**. Cleanups that belong to a phase
are listed inside it; the closing section lists what was inspected and
deliberately left alone.

Ground rules that apply to every item:

- Pre-v1, one backend in-tree: breaking the Go API is acceptable and
  expected. Two things are frozen and must stay byte-compatible: the
  manifest TOML/JSON format (and its generated schema) and the
  control-socket wire format, including its QMP-named JSON fields.
- Invariants from the roadmap hold: one-way imports (`backend` → `vm`,
  never the reverse), capabilities as standalone `-er` interfaces
  discovered by assertion (never added to the core interfaces), zero QEMU
  vocabulary outside `backend/qemu`, sealed unions over `any`.
- Follow AGENTS.md: `io`/`fs` interfaces over paths, evergreen tests of
  affirmative behavior, no timed sleeps, commit messages as
  `<component>: <short description>`.
- Each PR keeps `go build ./... && go test ./... && nix flake check`
  green, updates `example_test.go` and package docs to the new API, and
  adds a line to HISTORY.md for consumer-visible changes.
- Design references, when a shape question comes up: `net/http`'s server
  and `Shutdown`/`Close` split, `database/sql/driver`'s capability
  placement, `os/exec`'s `ExitError`, `x/net/nettest` and `testing/fstest`
  for conformance suites, `tsnet.Server` for zero-value-usable servers.

## Phase A — Contract shape

Land A1–A4 together: they change what everything later is written
against, and no in-tree caller outside `backend/qemu` moves.

### A1. Rename `Instance` to `Machine`

**Change.** `backend.Instance` → `backend.Machine`; `m` is the
conventional variable name. In `backend/qemu`, the config field `Machine
string` (the QEMU machine type) → `MachineType`, freeing `qemu.Machine`
for the exported concrete type (A4).

**Why.** "Instance" is generic and carries the spec-vs-running distinction
only by convention. The object is a machine: firecracker-go-sdk starts a
`Machine` from a `Config`; VirtualBox's API is `IMachine`; KubeVirt's
`VirtualMachine`/`VirtualMachineInstance` split is the same idea wearing
the generic word. Rejected: `VM` (the package is already `vm`), `Process`
(wrong for in-process backends, the reason the contract avoids process
vocabulary), `Domain` (libvirt jargon), `Sandbox` (names the purpose, not
the mechanism), `Guest`/`Session` (already mean other things here).
`MachineType` is QEMU's own vocabulary (`-machine type=`).

**Files.** `backend/backend.go`, `backend/qemu/qemu.go`,
`backend/qemu/spec.go`, `manifest/manifest.go`, `example_test.go`, docs.

**Done when.** No `Instance` identifier remains in exported API; the
example reads `m, err := b.Start(ctx, spec)`.

### A2. Capabilities move onto `Machine`

**Change.** Live-object capabilities are asserted on the machine and take
no machine argument; the one capability that *creates* a machine stays on
the backend. `stateDir` parameters are removed.

```go
// on the machine
type Suspender interface {
	// Suspend saves the machine's state to its state directory and stops it.
	Suspend(ctx context.Context) error
}
type MemoryResizer interface {
	ResizeMemory(ctx context.Context, size units.Bytes) error
}
type DeviceAttacher interface {
	Attach(ctx context.Context, dev vm.Device) error
	Detach(ctx context.Context, dev vm.Device) error
}
type ConsoleProvider interface { // unchanged
	Console(ctx context.Context) (vm.Term, error)
}

// on the backend, because it creates machines
type Resumer interface {
	// Resume restores the machine whose state the spec's state directory holds.
	Resume(ctx context.Context, spec *vm.Spec) (Machine, error)
	StateVersion() string
}
```

**Why.** Today `Suspender`, `MemoryResizer`, and `DeviceAttacher` hang off
the backend and take `inst Instance`, while `ConsoleProvider` is declared
on the instance: two conventions in one package. Every backend-level
method in `backend/qemu/qemu.go` opens with `b.ownInstance(inst)`, a
runtime type assertion ("instance was not started by this backend"), then
delegates to the instance's own `*vmm.VM` (`i.vm.Suspend`,
`i.vm.ResizeMemory`, `i.vm.AttachHotplugDevice`). `Suspend`'s `stateDir`
is rejected unless it equals the directory fixed at `Start`
(`i.vm.StateDir()`); `Resume` rejects any `stateDir` differing from the
spec-derived one. The parameter cannot do anything. No non-test code calls
these methods (the CLI reaches `vmm` via `backend/qemu/session`) and no
test does either, so churn is zero. `database/sql/driver` puts optional
interfaces on the live object (`driver.Conn`: `ConnBeginTx`, `Pinger`,
`SessionResetter`), not on `driver.Driver`.

```go
// before: keep b and m paired, hand one back to the other
if s, ok := b.(backend.Suspender); ok { err = s.Suspend(ctx, m, "") }

// after
if s, ok := m.(backend.Suspender); ok { err = s.Suspend(ctx) }
```

**Files.** `backend/backend.go`, `backend/qemu/qemu.go` (delete
`ownInstance`; move the four methods to the machine type),
`backend/backend_test.go`.

**Done when.** `ownInstance` and its error string are gone; capability
methods take no machine or `stateDir` argument; `qemu`'s machine type
asserts to `Suspender`, `MemoryResizer`, `DeviceAttacher`.

### A3. Graceful shutdown joins the machine contract

**Change.**

```go
type Machine interface {
	Wait(ctx context.Context) error
	Kill() error
	Shutdown(ctx context.Context) error // graceful; falls back to Kill on ctx expiry
	RemoteControl() (vm.Guest, error)
}
```

`backend.Shutdown` becomes a one-line alias for one release, then goes.

**Why.** The QEMU instance already implements a richer teardown than the
contract exposes: `(*instance).Close` (`backend/qemu/qemu.go`, over
`vmm.VM.Close`) runs guest shutdown, then QMP quit, then signals, then
runtime cleanup, and is idempotent. It is reachable only by type
assertion, so the `backend.Shutdown` helper re-implements a weaker version
(`RemoteControl → Guest.Shutdown → Wait`, else `Kill`) on the public
methods, and `backend/backend_test.go` (83 lines, the contract's only
test) tests just that helper against inline fakes. `net/http` is the
precedent: `Server.Shutdown(ctx)` graceful and `Server.Close()` hard, both
on the object.

```go
defer backend.Shutdown(ctx, m) // before
defer m.Shutdown(ctx)          // after
```

**Files.** `backend/backend.go`, `backend/qemu/qemu.go` (`Close` becomes
`Shutdown`), `backend/backend_test.go`, `example_test.go`.

**Done when.** `Shutdown` is on the interface and implemented by the QEMU
machine's existing teardown; the helper is an alias marked for removal.

### A4. Export the QEMU backend and machine types

**Change.** `Config`'s fields become fields of an exported `Backend`;
`New` is deleted; the machine type is exported; capabilities are
documented by compile-time assertions.

```go
package qemu

type Backend struct {
	Binary        string
	MachineType   string // QEMU machine type (-machine type=); was Machine
	// ... the remaining Config fields ...
	RemoteControl RemoteControl
	Logger        *slog.Logger // nil discards
}

func (b *Backend) Start(ctx context.Context, spec *vm.Spec) (backend.Machine, error)
func (b *Backend) Resume(ctx context.Context, spec *vm.Spec) (backend.Machine, error)
func (b *Backend) StateVersion() string

type Machine struct{ /* unexported fields */ }

var (
	_ backend.Backend        = (*Backend)(nil)
	_ backend.Resumer        = (*Backend)(nil)
	_ backend.Suspender      = (*Machine)(nil)
	_ backend.MemoryResizer  = (*Machine)(nil)
	_ backend.DeviceAttacher = (*Machine)(nil)
)
```

Fields must not be modified after `Start`; document it as `http.Server`
does. Defaults (logger, console output) apply at first use, not in a
constructor.

**Why.** `qemu.New(cfg) (backend.Backend, error)` cannot fail — its body
is `return newBackend(cfg, nil), nil` — so every caller writes a dead
error branch (the shipped `example_test.go` does). Its only in-tree
callers are that example and, via `NewBackendFromDocument`,
`manifest.Load`. The concrete types are unexported and there are no
`var _ backend.X` assertions, so pkg.go.dev shows nothing about which
capabilities QEMU supports. Returning an interface from a constructor
inverts "accept interfaces, return structs"; `http.Server` and
`tsnet.Server` are the shape that fixes it.

```go
// before
b, err := qemu.New(qemu.Config{RemoteControl: qemu.QGA{}})
if err != nil { return err } // never taken
m, err := b.Start(ctx, spec)

// after
b := &qemu.Backend{RemoteControl: qemu.QGA{}}
m, err := b.Start(ctx, spec)
```

Do not embed `*vmm.VM` in `qemu.Machine`: method promotion would leak
`qga.Client` through `vmm.VM.DialGuestAgent`. Delegate explicitly.

**Files.** `backend/qemu/qemu.go`, `manifest/manifest.go` (constructs a
`*qemu.Backend`; still returns `backend.Backend`), `example_test.go`.

**Done when.** `qemu.New` and `qemu.Config` are gone; `go doc
backend/qemu` lists `Backend`, `Machine`, and the assertions.

### Phase A cleanups

- Move the misplaced unit tests out of `vmm/manager_test.go` (5,594
  lines, 95 tests): `TestRemoveStaleSockets*`, `TestCreateVolumeImage*`,
  `TestWaitForSockets*`, `TestAllocateCID*` exercise `launch` functions and
  belong beside them; the twelve `TestBuildQEMUCommand*` tests are
  argument-lowering tests for `vmm/qemu.go` and belong in a `qemu_test.go`
  next to it.
- Delete test helpers unreachable even from tests (found with
  `deadcode -test`): `manifestWritePath`, `commandEnvAdditions`,
  `stringPtr`/`intPtr`/`boolPtr` in `vmm/manager_test.go`;
  `commandArguments` in `qmpclient/qmp_test.go`.

## Phase B — The control socket serves the `Machine` contract

Lands right after Phase A. It does not wait for the guest daemon.

**Change.** `control.Dial` returns a `backend.Machine` implemented over
the socket, so in-process and out-of-process control share one contract.

```go
func Dial(ctx context.Context, socketPath string) (backend.Machine, error)

type StatusReporter interface { // in backend
	Status(ctx context.Context) (Status, error)
}
```

- **Capabilities and skew.** Go interface assertions are static per type,
  but the proxy's real capabilities depend on the server it dialed. The
  proxy implements every capability and returns `errors.ErrUnsupported`
  from any the server's `methods` list lacks — the contract's existing
  rule for absent capabilities — so version skew is a runtime
  `ErrUnsupported`, never a compile-time surprise.
- **`Wait`/`Done`/`Err`** (with E). An additive `wait` RPC (Docker's
  container wait), issued in a goroutine at `Dial`; `Done` closes when it
  returns, `Err` holds the result. The server answers pending `wait` calls
  before tearing the socket down (the runtime's ordered close in
  `backend/qemu/internal/runtime/closer.go`). Socket EOF is the safety net
  for a server that dies without answering: `Done` closes with an "exited
  without status" error, refined from the on-disk launch record
  (`writeLaunchStats`) when present. Servers whose `methods` lack `wait`
  degrade to `status` polling plus EOF. When the `Events` capability lands,
  `exited` is one of its events; `wait` remains the simple call.
- **`status`** becomes `backend.StatusReporter`. `Status`'s JSON tags are
  the frozen wire names (`qmpSocket`, `guestAgentSocket`, `qmpReadyAt`, …)
  and its Go fields are neutral: bytes fixed, identifiers free. The QEMU
  machine implements it in-process (it has the paths, CID, PID, and stats)
  and the proxy implements it over the wire, so `virtle status` and
  `m.(backend.StatusReporter)` are one call.
- **`RemoteControl()`** on the proxy returns a `vm.Guest` over the
  socket's `guest-*` methods today; once the daemon exists it returns a
  client dialed straight to the daemon, so `guest-*` proxying ends and the
  socket narrows to host-session concerns without a wire break (roadmap
  item 5).
- **The CLI.** `virtle rpc` keeps `Raw` as the debugging escape hatch
  (`kubectl get --raw`, `virsh qemu-monitor-command` precedent). `virtle
  suspend`, `virtle hotplug`, and a new `virtle status` become thin typed
  calls over the proxy.

**Why.** The socket exists because `virtle suspend` runs in a different
process from the one that owns the machine — the machine-handle-across-
processes problem. Docker's Go client implements what dockerd serves and
Tailscale's `LocalClient` mirrors `LocalBackend`; neither builds its CLI on
a raw passthrough. Today `internal/control` is a bespoke JSON-RPC
client/server (`status`, `methods`, `suspend`, `hotplug`, `balloon`,
`guest-*`); `virtle rpc` uses `Raw`, `vmm/control.go` uses `Suspend`, and
the other typed client methods are unreachable from production (called
only from `manager_test.go`'s control-periphery tests). The wire method
names map one to one onto `Machine` and capability methods, so the Go
client changes shape while the bytes do not.

```go
// before: bespoke, out-of-process only
session.Suspend(ctx, mf) // → vmm → control.Dial(path).Suspend(ctx, control.SuspendRequest{})

// after: the same contract in and out of process
m, err := control.Dial(ctx, socketPath)
if s, ok := m.(backend.Suspender); ok { err = s.Suspend(ctx) }
```

**Files.** `internal/control/client.go` (the ad-hoc typed methods are
replaced by the `Machine` implementation, not kept alongside it),
`internal/control/types.go` (`Status` struct, `wait` request/response),
`backend/backend.go` (`StatusReporter`, `Status`), the control server in
`backend/qemu/internal/runtime`, `backend/qemu/qemu.go` (in-process
`Status`), `main.go` (`suspend`/`hotplug`/`status` over the proxy).

**Done when.** `main.go` performs `suspend` and `hotplug` through
`control.Dial(...).(backend.Suspender)` etc.; `virtle status` exists; the
`session.Suspend`/`session.Hotplug` delegates are deleted;
`backendtest.TestBackend` (G) runs against the proxy; the wire format's
existing methods are byte-identical (existing transport tests pass
unchanged).

### Phase B cleanups

- The `internal/control.Client` typed methods `Status`, `Methods`,
  `Balloon`, `GuestPS`, `GuestExec`, `GuestRead`, `GuestWrite` go with the
  replacement; tests that used them use the `Machine` proxy.
- `fakeControlCore` is defined in both `internal/control` and `vmm` tests;
  keep one.

## Phase C — `Guest.Run` gets the `os/exec` contract

**Change.**

```go
type GuestCmd struct {
	Path           string
	Args, Env      []string
	Dir            string
	Stdin          io.Reader
	Stdout, Stderr io.Writer // nil discards
}

// Run runs the command to completion. A non-zero exit status is returned
// as an error satisfying errors.As(err, *vm.ExitError).
func (Guest) Run(ctx context.Context, cmd *GuestCmd) error

type ExitError struct{ Code int; Stderr []byte } // Stderr: tail, when not otherwise captured
func (e *ExitError) Error() string

// Output runs cmd and returns its standard output (the exec.Cmd.Output analog).
func Output(ctx context.Context, g Guest, cmd *GuestCmd) ([]byte, error)
```

`GuestResult` is deleted. The QGA adapter writes its buffered output to
the writers at completion; the guest daemon streams as bytes arrive.
"Streaming exec" stops being a future extension — it is the same call.

**Why.** Today `Run` returns `GuestResult{ExitCode, Stdout, Stderr}`, so a
non-zero exit is not an error. The footgun is live: `guestFileWriter.Close`
in `backend/qemu/guest.go` applies file mode by running `chmod` through
`Run` and checks only `err`, so a failing `chmod` (exit 1) is silently
ignored. The QGA adapter already lowers `Dir` and `Env` onto a `/bin/sh
-c` wrapper and rejects `Stdin` with `errors.ErrUnsupported`; its output
is buffered because `guest-exec-status` delivers it at completion. Both
`exec.Cmd` and `x/crypto/ssh.Session` take writers and return
`*ExitError`, which is why `if err != nil` suffices there.

```go
// before: exit code checked by hand, and usually isn't
res, err := g.Run(ctx, &vm.GuestCmd{Path: "make", Dir: "/workspace"})
if err != nil { return err }
if res.ExitCode != 0 { return fmt.Errorf("make: exit %d", res.ExitCode) }
os.Stdout.Write(res.Stdout)

// after
err := g.Run(ctx, &vm.GuestCmd{Path: "make", Dir: "/workspace", Stdout: os.Stdout})
if err != nil { return err } // includes non-zero exit
```

**Files.** `vm/guest.go`, `backend/qemu/guest.go` (adapter and the
`chmod` call, which now propagates a non-zero exit), `example_test.go`,
`vm/guest_test.go`.

**Done when.** `GuestResult` is gone; a test proves a non-zero exit from
the QGA adapter surfaces as `*vm.ExitError`; the `chmod` failure in
`guestFileWriter.Close` is an error.

## Phase D — Mechanical type fixes

Independent of each other; one PR or several.

### D1. One `units` package, doubled down on

**Change.** `units` is the single, public, reusable home for unit-related
logic — types, constants, parsing, formatting, encoding — and the manifest
is a consumer of it.

- Delete `internal/units`; every importer moves to `units`.
- Roles, stated in the package doc: `Bytes` is the API type (all of
  `vm`/`backend` speak it). `MiB` is retained as the *denominated codec
  type* for encoded fields whose format counts in MiB, with lossless
  conversion both ways (`MiB.Bytes() Bytes`, `Bytes.Mebibytes() MiB`).
  `Duration` is the encoding-aware duration for encoded fields (bare
  numbers mean seconds); method parameters that never cross an encoding
  boundary use `time.Duration` (`http.Server.ReadTimeout` precedent) and
  convert via `.Duration()`.
- `Bytes` gains `ParseBytes("2GiB")`, `MarshalText`/`UnmarshalText`
  (string form with unit suffix), JSON and TOML codecs built on them, and
  accessors matching `Mebibytes()` (`Kibibytes`, `Gibibytes`).
- `Duration` gains `UnmarshalText`; both types document one grammar for
  the strings they accept.
- The schema hints for `Duration` and `MiB` move next to the types, so
  `internal/manifest/schema/schema.go` stops reflecting on them by hand.

**Why.** Two packages exist: public `units` (`Bytes` + `Duration`) and
`internal/units` (the legacy `MiB int` count type with `Bytes()`/`Int()`
plus a byte-identical copy of `Duration`). Every internal importer
(`internal/manifest/*`, `internal/control/types.go`,
`backend/qemu/spec.go`) still uses `internal/units`; nothing under
`internal/` imports the public one. The manifest's size fields (`Memory`,
image `Size`, balloon thresholds) are `units.MiB` because their TOML
contract is a bare number in MiB, and `manifest.Load` converts by hand
(`units.Bytes(mount.Image.Size.Bytes())`). `Bytes` has `String()` and
nothing else, so it cannot round-trip `"2GiB"` through any encoding.

```go
// before: two packages, conversion by hand
Memory iunits.MiB `toml:"memory"`             // internal/manifest
spec.Memory = units.Bytes(doc.Memory.Bytes()) // manifest.Load

// after: one package, typed conversion
Memory units.MiB `toml:"memory"`
spec.Memory = doc.Memory.Bytes()
```

**Files.** `units/*`, delete `internal/units`, importers listed above,
`internal/manifest/schema/schema.go`. `vm.Spec` and `backend` keep
`units.Bytes` unchanged.

**Done when.** `internal/units` does not exist; `units.Bytes` round-trips
`"2GiB"` through text, JSON, and TOML in tests; the manifest's MiB fields
decode bare numbers exactly as before (existing manifest tests pass).

### D2. `Config.KVM *bool` becomes an `Accel` enum

**Change.**

```go
type Accel string
const (
	AccelAuto Accel = ""    // KVM when available, else TCG
	AccelKVM  Accel = "kvm"
	AccelTCG  Accel = "tcg"
)
```

```go
kvm := false
b := &qemu.Backend{KVM: &kvm}           // before
b := &qemu.Backend{Accel: qemu.AccelTCG} // after
```

**Why.** `backend/qemu/spec.go` copies the pointer into
`doc.Machine.KVM *bool`; the three states are auto (nil), on, off. A
pointer-bool tri-state is exactly what the sealed-union rule exists to
avoid; the manifest's own tri-state maps trivially (auto ↔ nil).

**Files.** `backend/qemu/qemu.go`, `backend/qemu/spec.go`.

**Done when.** No `*bool` remains in `qemu.Backend`.

### D3. `Forward.Proto` gets a type; endpoints stay strings

**Change.**

```go
type Proto string
const (
	TCP Proto = "tcp" // zero value of Forward.Proto means TCP
	UDP Proto = "udp"
)
type Forward struct {
	HostAddr  string // "host:port" or ":port"; hostnames allowed
	GuestAddr string
	Proto     Proto
}
```

**Why.** `Proto` is a free-form string defaulted to `"tcp"` in two places
(`backend/qemu/spec.go`; `manifest/manifest.go` passes the manifest's
string through). `netip.AddrPort` was considered for the endpoints and
rejected: `parsePortEndpoint` in `internal/manifest/resolve_manifest.go`
accepts an empty host (`":8080"` binds all interfaces) and never parses
the address as an IP, so hostnames are legal; `netip` would silently
narrow both. Leave the endpoint type alone until a use case needs more
than `address:port`.

**Files.** `vm/spec.go`, `backend/qemu/spec.go`, `manifest/manifest.go`.

**Done when.** `Forward.Proto` is typed; the endpoint grammar is in the
field docs; both defaulting sites use `vm.TCP`.

### D4. Free renames while unimplemented

**Change.**

- `TermOptions.TERM` → `TermType`.
- `CopyOptions{UID, GID *int}` → `CopyOptions{Chown bool; UID, GID int}`.

**Why.** `vm.GuestWithCopy`, `CopyOptions`, `vm.Term`, and `TermOptions`
have no implementation and no callers yet (roadmap items 2 and 3), so
these cost nothing now and something later. The all-caps field reads as a
shouted type name beside `vm.Term`. The `os.Chown`-shaped gate says the
same thing as the pointers ("false keeps the archive's recorded owners")
without a pointer to explain.

**Files.** `vm/term.go`, `vm/guest.go`, `guest.md` (update the
`CopyOptions` sketch).

**Done when.** Both renames applied; `guest.md` matches.

## Phase E — `Machine` exposes `Done` and `Err`

Lands with the first step of the session rebuild (roadmap item 4), which
is its consumer; the `wait` RPC from Phase B is its remote implementation.

**Change.** The `context.Context` shape, with `Wait` kept as sugar.

```go
type Machine interface {
	Done() <-chan struct{}          // closed when the VM has exited and runtime state is released
	Err() error                     // exit error, valid after Done is closed
	Wait(ctx context.Context) error // select over Done and ctx
	// ...
}
```

```go
// before: a goroutine per waiter
go func() { errc <- m.Wait(ctx) }()
select { case err := <-errc: ...; case <-sig: ... }

// after
select {
case <-m.Done():
	err := m.Err()
case <-sig:
	m.Shutdown(ctx)
}
```

**Why.** Exit is observable only by blocking in `Wait`. No code outside
`backend/qemu` calls it; the CLI's foreground loop lives in `vmm` because
it must `select` over VM exit, signals, suspend requests, and session
events, and the contract offers nothing selectable. The plumbing exists:
`internal/executor.Process` has `Done() <-chan struct{}`, and `vmm.VM.Wait`
is a thin wrapper over `Process.WaitContext`.

A machine-level `Events(ctx) iter.Seq[Event]` capability (boot, guest
ready, suspended, exited) is the natural follow-on for the session
rebuild; it is not part of this item.

**Files.** `backend/backend.go`, `backend/qemu/qemu.go`,
`backend/qemu/internal/vmm/api.go`, the control proxy (B).

**Done when.** The rebuilt CLI loop selects on `Done()`; the proxy's
`Done` is driven by `wait`.

### Phase E cleanups (with roadmap item 4)

These are deleted with the session layer, not refactored first:

- `vmm/ssh_ready.go` and `vmm/ssh_autoprovision.go`, which re-wrap
  `launch/ssh_ready.go` and `launch/ssh_autoprovision.go` (both layers
  import `readiness` and `sshtools`).
- The `VIRTLE_SSH_READY_TIMEOUT` environment knob read inside library code
  (`readiness.TimeoutFromEnv`): configuration by environment variable in a
  library is a smell, and the daemon handshake makes it moot. Do not port.
- `vmm.VM.DialGuestAgent` and `ShutdownGuest`, which have one caller each
  afterwards (the QGA transport) and belong inside it.
- `session.Suspend`/`Hotplug`/`ExitCode`, one-line delegates that exist
  only because `vmm` is internal (B already replaces the first two).
- Session-era tests in `vmm/manager_test.go`: SSH readiness, retry pacing,
  autoprovision, SIGUSR1 info, connect-hint printing
  (`TestManagerLaunchStartsSSHOnceAfterReadiness`,
  `…PacesSSHRetriesAndWarnsAfterFiveFailures`, `TestWaitForSSHReady*`,
  `TestNewManagerUsesSSHReadyTimeoutEnv`,
  `…AutoprovisionsSSHKeyAfterAuthFailure`, `…PrintsGuestInfoOnSIGUSR1`,
  …). The rebuilt loop is tested against `backendtest.Backend` (G).
- Bug-lock tests that name a fixed defect rather than a behavior
  (`TestManagerStartQEMUNilRunnerWrapsOnce`,
  `TestNewManagerIgnoresInvalidSSHReadyTimeoutEnv`,
  `…HandlesDuplicateSuspendDuringActiveSessionWithoutForwardingJobControl`):
  fold each into the affirmative test of its feature, or delete it when
  the feature goes.
- Duplicated fakes: `fakeQMPDialer`, `fakeGuestAgentDialer`,
  `fakeGuestAgentClient`, `fakeSSHReadyDialer`, `fakePIDSignaler` are each
  defined in both `vmm/manager_test.go` and `launch/*_test.go`. Consolidate
  into one doubles package per seam, as `executortest` already does for
  the process seam.

## Phase F — `manifest.Load` dogfooding and the overlay contract

Lands with manifest consolidation (roadmap item 1).

**Change.**

1. Make the CLI a consumer of `manifest.Load` for `virtle launch`, the
   same "first consumer" discipline the rest of the API got.
2. After consolidation, `Load` returns a Spec and a backend that are
   complete: no document carried inside the backend, no overlay rule.
   Until then the overlay stays documented as transitional.

**Why.** `manifest.Load` has no in-tree consumer: the CLI loads through
`internal/manifest` and hands the internal `*Manifest` to
`backend/qemu/session`. The public loader is exercised only by its tests,
and its documented overlay semantics ("edits to the Spec are overlaid when
the backend starts") are a contract nothing depends on and nothing
verifies end to end.

**Files.** `main.go`, `manifest/manifest.go`, `backend/qemu/qemu.go`.

**Done when.** `virtle launch` goes through `manifest.Load`;
`NewBackendFromDocument` and the backend's `doc` field are gone.

### Phase F cleanups (with roadmap item 1)

- The bidirectional converter pair `backend/qemu/spec.go` (Spec →
  Document, 249 lines) and `manifest.specFromDocument` (Document → Spec)
  collapses to one lowering direction.
- `manifest_test.go`'s QEMU-section tests (`TestManifestValidatesQEMU*`,
  `TestKernelSerialModesResolveToQEMUConsole`,
  `TestDocumentImageMountFormatResolvesToQEMU`) move with the `[qemu]`
  resolution.

## Phase G — Test packages the x/ tree would ship

Tier 2 lands with the session rebuild that consumes it (E); tier 1 lands
with the flake integration extension, or earlier as insurance for a
second backend.

**Change.** Two tiers with opposite needs, sharing one contract.

*Tier 1 — conformance (e2e, opt-in only).* A suite that boots real
machines from whatever backend it is handed, the way `fstest.TestFS` reads
real files and `nettest.TestConn` opens real sockets:

```go
// backend/backendtest — the nettest.TestConn / fstest.TestFS of this API.
// The factory returns a backend and a Spec that boots on it; each sub-test
// starts a fresh machine, since suspend and hotplug are destructive.
// Capabilities are discovered by assertion and skipped when absent.
func TestBackend(t *testing.T, start func(t *testing.T) (backend.Backend, *vm.Spec))
```

`backendtest` carries no build tag (it is a library). Its QEMU wiring
follows the repo's existing opt-in convention exactly:

```go
//go:build integration

package qemu_test // backend/qemu/backend_integration_test.go

func TestIntegrationBackend(t *testing.T) {
	backendtest.TestBackend(t, func(t *testing.T) (backend.Backend, *vm.Spec) {
		return &qemu.Backend{RemoteControl: qemu.QGA{}}, integrationSpec(t)
	})
}
```

Run locally with `go test -tags=integration ./...`; in CI the
`integration` flake check grows a second tagged test binary
(`backend/qemu`) plus the kernel/initrd/agent image `integrationSpec`
boots, under TCG where the check's VM has no KVM. Until `qemu.Guest{}`
exists, the `RemoteControl` sub-tests run against QGA. The suite also runs
against the control proxy (B).

*Tier 2 — doubles (unit, always on).* For consumers who must never boot a
VM:

```go
// vm/vmtest — the fstest.MapFS of vm.Guest: an in-memory Guest with a
// MapFS-backed filesystem and scripted command results.
type Guest struct {
	FS       fstest.MapFS
	Commands map[string]Result // keyed by GuestCmd.Path
}

// backendtest.Backend — an in-memory backend.Backend whose machines boot
// instantly, close Done on Kill/Shutdown, and return a vmtest.Guest from
// RemoteControl. What the rebuilt CLI loop and library users test against.
type Backend struct{ Guest *vmtest.Guest }
```

The tiers check each other: `backendtest`'s own untagged tests run
`TestBackend` against `backendtest.Backend` in milliseconds — how `nettest`
validates itself against `net.Pipe` — so the suite stays honest without a
VM and the fake is held to the same contract as QEMU.

**Why.** `backend/backend_test.go` tests only the `Shutdown` helper
against inline fakes; `example_test.go` cannot execute (it needs a kernel
image); a library consumer has no way to unit-test code that takes a
`vm.Guest` without QEMU; a second backend would have no conformance check.
The existing opt-in precedent is
`backend/qemu/internal/launch/guest_dir_install_integration_test.go`, run
by the `integration` flake check inside a small Linux VM; nothing runs it
by accident.

**Files.** New `backend/backendtest`, new `vm/vmtest`,
`backend/qemu/backend_integration_test.go`, `flake.nix` (integration
check).

**Done when.** `go test ./...` runs the suite against the fake; `go test
-tags=integration ./backend/qemu/` runs it against QEMU; `nix flake check`
includes the latter.

## Inspected and deliberately kept

So nobody re-litigates them:

- `internal/executor/executortest`: flagged by `deadcode` only because it
  is test-only; 15 test files use it.
- The manifest test case named "legacy working dir": it covers the
  zero-value `RuntimeDir` mode, not a compatibility shim.
- `vmm.StartVM`: the library path's entry point, unreachable only from
  `main`.
- `virtle rpc` using `Raw`: the intended escape hatch (B).

## Sequence summary

| Order | Phase | Ships with |
|---|---|---|
| 1 | A (A1–A4 + cleanups) | one PR |
| 2 | B | one PR; needs A, not the daemon |
| 3 | C | one PR; fixes the live `chmod` bug |
| 4 | D1–D4 | one PR or several; independent |
| 5 | E (+ cleanups) | roadmap item 4, first step |
| 6 | F (+ cleanups) | roadmap item 1 |
| 7 | G | tier 2 with E; tier 1 with the flake extension |

Every step keeps `go build ./... && go test ./... && nix flake check`
green and is separately landable.
