# Library API: improvements to shipped surface

Status: proposal, sibling to [roadmap.md](roadmap.md) (new functionality)
and [guest.md](guest.md) (the guest daemon); refs
[#66](https://github.com/shazow/virtle/issues/66),
[#67](https://github.com/shazow/virtle/pull/67).

This document plans changes to the public API that already shipped in
v0.3.x (`vm`, `backend`, `backend/qemu`, `manifest`, `units`). Every item
was traced against actual call sites in the tree before being proposed;
the trace is recorded with each item because it is what justifies the
change. The module is pre-v1 with one backend in-tree, so all of these are
as cheap now as they will ever be. Design references: the extended
standard library (`x/net/nettest`, `testing/fstest`, `os.Root`),
`net/http`'s server shape, `database/sql/driver`, and `tsnet`.

Sequencing is at the end; items 0–3 change what everything later is
written against and should land first.

## 0. `Instance` is renamed `Machine`

**Today.** `backend.Backend.Start` returns a `backend.Instance`.

**Trace.** "Instance" is generic and carries the spec-vs-running
distinction only by convention. The thing is a machine: firecracker-go-sdk
starts a `Machine` from a `Config`, VirtualBox's API is `IMachine`, and
KubeVirt's `VirtualMachine` (spec) / `VirtualMachineInstance` (running)
split is the same idea wearing the generic word. `VM` loses because the
package is already named `vm`; `Process` is wrong for in-process backends
(the reason the contract avoids process vocabulary); `Domain` is libvirt
jargon; `Sandbox` names the purpose, not the mechanism; `Guest` and
`Session` already mean other things here. One collision inside
`backend/qemu`: `Config.Machine string` is the QEMU machine type, and
becomes `MachineType` — QEMU's own vocabulary (`-machine type=`), freeing
`qemu.Machine` for the exported concrete type (item 3).

**Proposal.** `backend.Machine`, with `m` as the conventional variable
name; `vm.Spec` → `backend.Machine` reads as description → running
machine without a qualifier. Everything below uses the new name in
proposals and the old name when describing today's code.

```go
m, err := b.Start(ctx, spec)
defer m.Shutdown(ctx)
```

## 1. Capabilities move onto `Machine`

**Today.** `backend.Suspender`, `MemoryResizer`, and `DeviceAttacher` are
asserted on the *backend* and take `inst Instance` as an argument;
`ConsoleProvider` is declared on the *instance*. Two conventions in one
package.

**Trace.** Every backend-level capability method in `backend/qemu/qemu.go`
opens with `b.ownInstance(inst)` — a runtime type assertion that errors
with "instance was not started by this backend" — then delegates to a
method on the instance's own `*vmm.VM` (`i.vm.Suspend`,
`i.vm.ResizeMemory`, `i.vm.AttachHotplugDevice`). `Suspend`'s `stateDir`
parameter is rejected unless it equals the directory fixed at `Start`
(`i.vm.StateDir()`), and `Resume` likewise rejects a `stateDir` that
differs from the one the spec derives — the parameter cannot do anything.
No non-test code calls these methods (the CLI reaches `vmm` directly
through `backend/qemu/session`), and no test calls them either, so the
change has zero in-tree churn. `database/sql/driver`, the stated model,
puts optional interfaces on the live object (`driver.Conn`:
`ConnBeginTx`, `Pinger`, `SessionResetter`), not on `driver.Driver`.

**Proposal.** Live-object capabilities on `Machine`; the one capability
that *creates* a machine stays on the backend.

```go
// on the live object
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

`ownInstance` and its error class disappear; the compiler does the pairing
check the runtime did. `stateDir` goes: state lives where the spec says.

Before / after:

```go
// before: keep b and m paired, pass one back to the other
if s, ok := b.(backend.Suspender); ok {
	err = s.Suspend(ctx, m, "")
}

// after
if s, ok := m.(backend.Suspender); ok {
	err = s.Suspend(ctx)
}
```

## 2. Graceful shutdown is part of the machine contract

**Today.** `Instance` has `Wait`, `Kill`, `RemoteControl`. Graceful stop
is a package helper, `backend.Shutdown(ctx, m)`, which does
`RemoteControl → Guest.Shutdown → Wait`, falling back to `Kill`.

**Trace.** The QEMU instance already implements a richer teardown that the
interface hides: `(*instance).Close` runs guest shutdown, then QMP quit,
then signals, then runtime cleanup, and is safe to call twice
(`backend/qemu/qemu.go`, `vmm.VM.Close`). It is reachable only by type
assertion, so the helper re-implements a worse version on top of the
public methods, and `backend/backend_test.go` (83 lines, the only test of
the contract) tests just that helper against ad-hoc fakes. `net/http` is
the precedent: `Server.Shutdown(ctx)` graceful, `Server.Close()` hard —
both on the object.

**Proposal.**

```go
type Machine interface {
	Wait(ctx context.Context) error
	Kill() error
	Shutdown(ctx context.Context) error // graceful; falls back to Kill on ctx expiry
	RemoteControl() (vm.Guest, error)
}
```

`backend.Shutdown` becomes a one-line alias for a release, then goes.
Before / after:

```go
defer backend.Shutdown(ctx, m)   // before
defer m.Shutdown(ctx)            // after
```

## 3. Export the QEMU backend and machine types

**Today.** `qemu.New(cfg Config) (backend.Backend, error)` returns an
interface wrapping unexported `qemuBackend`; instances are unexported
`instance`.

**Trace.** `New` cannot fail — its body is `return newBackend(cfg, nil),
nil` — so every caller writes an error branch that never runs (the shipped
`example_test.go` does). Its only in-tree callers are that example and,
through the sibling `NewBackendFromDocument`, `manifest.Load`. Because the
concrete types are unexported and there are no `var _ backend.X =`
assertions, pkg.go.dev shows *nothing* about which capabilities QEMU
supports; a reader must open the source. Returning an interface from a
constructor is the inverse of "accept interfaces, return structs", and
`http.Server` / `tsnet.Server` show the shape that fixes it: an exported
struct with exported config fields, zero value usable, defaults applied at
first use.

**Proposal.** `Config`'s fields become fields of an exported `Backend`;
`New` is deleted; the machine type is exported so its capabilities are
documented by assertion.

```go
package qemu

type Backend struct {
	Binary        string
	MachineType   string // QEMU machine type (-machine type=); was Machine
	// ... today's Config fields ...
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

Fields must not be modified after `Start`, documented as `http.Server`
does. Before / after:

```go
// before
b, err := qemu.New(qemu.Config{RemoteControl: qemu.QGA{}})
if err != nil { return err } // never taken
m, err := b.Start(ctx, spec)

// after
b := &qemu.Backend{RemoteControl: qemu.QGA{}}
m, err := b.Start(ctx, spec)
```

`manifest.Load` keeps returning `backend.Backend`; internally it
constructs a `*qemu.Backend` the same way.

## 4. `Guest.Run` gets the `os/exec` contract

**Today.** `Run(ctx, *GuestCmd) (*GuestResult, error)` with
`GuestResult{ExitCode, Stdout, Stderr []byte}`. A non-zero exit is not an
error.

**Trace.** The footgun is already live in the tree:
`guestFileWriter.Close` in `backend/qemu/guest.go` applies file mode by
running `chmod` through `Run` and checks only `err` — a failing `chmod`
(exit 1) is silently ignored. The QGA adapter already lowers `Dir` and
`Env` onto a `/bin/sh -c` wrapper and rejects `Stdin` with
`errors.ErrUnsupported`; output is buffered because `guest-exec-status`
delivers it at completion. Both `exec.Cmd` and `x/crypto/ssh.Session`
take `Stdout`/`Stderr` writers and return `*ExitError` for non-zero exit,
which is why `if err != nil` is sufficient there.

**Proposal.**

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

// Output runs cmd and returns its standard output, the exec.Cmd.Output analog.
func Output(ctx context.Context, g Guest, cmd *GuestCmd) ([]byte, error)
```

The QGA adapter writes its buffered output to the writers at completion;
the guest daemon streams as bytes arrive. "Streaming exec" stops being a
future `GuestWithX` extension — it is the same call. `GuestResult` is
deleted. Before / after:

```go
// before: exit code must be checked by hand, and usually isn't
res, err := g.Run(ctx, &vm.GuestCmd{Path: "make", Dir: "/workspace"})
if err != nil { return err }
if res.ExitCode != 0 { return fmt.Errorf("make: exit %d", res.ExitCode) }
os.Stdout.Write(res.Stdout)

// after
err := g.Run(ctx, &vm.GuestCmd{Path: "make", Dir: "/workspace", Stdout: os.Stdout})
if err != nil { return err } // includes non-zero exit
```

## 5. `Machine` exposes `Done` and `Err`

**Today.** Exit is observable only by blocking in `Wait(ctx)`.

**Trace.** No code outside `backend/qemu` calls `Wait` today; the CLI's
foreground loop lives in `vmm` because it must `select` over VM exit,
signals, suspend requests, and SSH-session events, and the public contract
offers nothing selectable. The plumbing is already there:
`internal/executor.Process` has `Done() <-chan struct{}`, and
`vmm.VM.Wait` is a thin wrapper over `Process.WaitContext`. The roadmap's
session-layer demolition ("machinery emits events, the CLI selects")
needs exactly this.

**Proposal.** The `context.Context` shape, with `Wait` kept as sugar:

```go
type Machine interface {
	Done() <-chan struct{} // closed when the VM has exited and runtime state is released
	Err() error            // exit error, valid after Done is closed
	Wait(ctx context.Context) error // select over Done and ctx
	// ...
}
```

Before / after, the shape the CLI loop takes once rebuilt on the public API:

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

A machine-level `Events(ctx) iter.Seq[Event]` capability (boot, guest
ready, suspended, exited) is the natural follow-on for the session
rebuild; it is not part of this item.

## 6. One `units` package, with `Duration` demoted

**Today.** `units` (public) and `internal/units` are byte-identical
copies; both export `Bytes` and `Duration`.

**Trace.** Every internal importer — `internal/manifest/*`,
`internal/control/types.go`, `backend/qemu/spec.go` — still imports
`internal/units`; nothing under `internal/` imports the public copy.
`units.Duration` appears nowhere in `vm`, `backend`, or `backend/qemu`'s
exported surface: it exists so TOML bare numbers can mean seconds
(`UnmarshalTOML`) and for the control socket's wire types. `units.Bytes`
lacks the text marshalling `Duration` has, so it cannot round-trip through
TOML/JSON as `"2GiB"`.

**Proposal.** Delete `internal/units`; public types speak `time.Duration`;
`Duration` becomes a manifest/control encoding detail living with the
manifest decoder (`internal/manifest`, later public `manifest` per the
roadmap); `units` keeps `Bytes` and gains the symmetry:

```go
func ParseBytes(s string) (Bytes, error)         // "2GiB", "512MiB", "1536B"
func (b Bytes) MarshalText() ([]byte, error)
func (b *Bytes) UnmarshalText(text []byte) error
```

The manifest's MiB-denominated bare numbers stay a manifest rule,
implemented in its decoder, not in `Bytes`.

## 7. `Config.KVM *bool` becomes an `Accel` enum

**Trace.** `backend/qemu/spec.go` copies the pointer into
`doc.Machine.KVM *bool`; the three states are auto (nil), on, off. A
pointer-bool tri-state is the pattern the review already ruled against
for `CopyOptions` and elsewhere.

**Proposal.**

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
b := &qemu.Backend{KVM: &kvm}        // before
b := &qemu.Backend{Accel: qemu.AccelTCG} // after
```

## 8. `Forward.Proto` gets a type; endpoints stay strings

**Trace.** The review floated `netip.AddrPort` for `Forward`'s endpoints.
Checking the actual rules (`parsePortEndpoint` in
`internal/manifest/resolve_manifest.go`) says no: endpoints accept an
empty host (`":8080"` binds all interfaces) and are never parsed as IPs,
so hostnames are legal. `netip.AddrPort` would silently narrow both.
`Proto`, however, is a free-form string defaulted to `"tcp"` in two
places (`spec.go`, and `manifest.go` passes the manifest's string through).

**Proposal.** Type the protocol, document the endpoint grammar, leave the
endpoint type alone until a use case needs more than `address:port`.

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

## 9. `CopyOptions` and `TermOptions`: free renames while unimplemented

**Trace.** `vm.GuestWithCopy`, `CopyOptions`, `vm.Term`, and `TermOptions`
have no implementation and no callers yet (roadmap items 2 and 3), so
these changes cost nothing today and something later.

- `TermOptions.TERM` → `TermType`: the all-caps field reads as a shouted
  type name beside `vm.Term`.
- `CopyOptions{UID, GID *int}` → `CopyOptions{Chown bool; UID, GID int}`:
  the `os.Chown`-shaped gate says the same thing ("false keeps the
  archive's recorded owners") without a pointer to explain. Optional
  values elsewhere can follow the same pattern or a `types/opt`-style
  wrapper if they multiply.

## 10. `manifest.Load` dogfooding and the overlay contract

**Trace.** `manifest.Load` has no in-tree consumer: the CLI loads through
`internal/manifest` directly and hands the internal `*Manifest` to
`backend/qemu/session`. So the public loader is exercised only by its
tests, and its documented overlay semantics ("edits to the Spec are
overlaid when the backend starts") are a contract nothing in the repo
depends on and nothing verifies end to end.

**Proposal.** Two steps, both prerequisites of roadmap item 1 rather than
substitutes for it:

1. Make the CLI a consumer of `manifest.Load` for `virtle launch` — the
   same "first consumer" discipline the rest of the API got — so the
   public path is the tested path.
2. Post-consolidation, `Load` returns a Spec and a backend that are
   *complete*: no document carried inside the backend, no overlay rule to
   explain. Until then, the overlay stays documented as transitional.

## 11. Test packages the x/ tree would ship

**Trace.** `backend/backend_test.go` tests only the `Shutdown` helper,
against ad-hoc fakes written inline; `example_test.go` cannot execute
(it needs a kernel image); a library consumer has no way to unit-test
code that takes a `vm.Guest` without QEMU; and a second backend would
have no conformance check.

**Proposal.**

```go
// backend/backendtest — the nettest.TestConn / fstest.TestFS of this API.
func TestBackend(t *testing.T, newBackend func(t *testing.T) backend.Backend)

// vm/vmtest — the fstest.MapFS of vm.Guest: an in-memory Guest with a
// MapFS-backed filesystem and scripted command results.
type Guest struct {
	FS       fstest.MapFS
	Commands map[string]Result // keyed by GuestCmd.Path
}
```

`backendtest` exercises Start/Wait/Done/Kill/Shutdown/RemoteControl and
each capability an implementation asserts to; `vmtest.Guest` gives
consumers a deterministic double. Both are small and pay for themselves
the first time a second backend or a downstream consumer appears.

## Sequencing

All items are breaking, which is acceptable pre-v1; each lands separately
with `go build ./... && go test ./... && nix flake check` green.

1. Items 0–3 together (contract shape: the `Machine` rename, capabilities on `Machine`,
   `Shutdown` in the contract, exported QEMU types) — they change what
   everything after is written against, and no in-tree caller moves.
2. Item 4 (`Run` contract) — fixes the live `chmod` bug in passing.
3. Items 6–9 (units, `Accel`, `Proto`, renames) — mechanical.
4. Item 5 (`Done`/`Err`) alongside the first step of the session rebuild
   (roadmap item 4), which is its consumer.
5. Item 10 with roadmap item 1; item 11 whenever the first of the two
   consumers it serves (a second backend, an external library user)
   appears, or earlier as insurance.
