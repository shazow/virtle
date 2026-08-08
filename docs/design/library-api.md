# Design: a public library API for virtle

Status: proposal (refs [#66](https://github.com/shazow/virtle/issues/66))

## Goal

Make it possible to build a "minimal virtle" by using virtle components as a
library, with an API designed from scratch rather than by promoting the
current `./internal` packages in place. Three requirements shape the design:

1. **Library-first.** A Go program should boot a VM, run commands in it, and
   tear it down in a handful of lines, without the CLI.
2. **The VM backend is abstracted.** QEMU is the only backend today, but the
   API must leave room for others — notably libkrun, which is an *in-process
   C library*, not an exec'd binary. The backend seam therefore must not
   assume processes, command lines, or host sockets.
3. **The guest agent is abstracted.** Guest operations run over QGA today,
   but QGA will ultimately be replaced by a virtle-native guest daemon
   (`virtle guest`). Guest operations must not leak QGA protocol shapes, and
   most functionality will be built around the expectation that VMs run the
   virtle guest daemon — while still working (degraded) when they don't.

## Design principles

- **stdlib-shaped.** The design mirrors the `database/sql` /
  `database/sql/driver` split: a consumer-facing package (`vm`) and an
  implementer-facing contract package (`backend`), where optional
  functionality is declared through small capability interfaces
  (`backend.Suspender`, ...) discovered by type assertion — the way
  `driver.Conn` implementations opt into `driver.ConnBeginTx`. Config is
  plain structs with usable zero values, not functional options. Blocking
  calls take a `context.Context` first. Data flows through `io` interfaces.
  Errors use sentinels plus `errors.Is`, and absent capabilities return
  `errors.ErrUnsupported`.
- **Interfaces are discovered, not designed.** `backend.Backend` and
  `vm.Guest` are cut to the minimum provably needed today; everything else
  is a capability interface, so adding functionality never breaks
  implementers.
- **The Spec is neutral and constructable in Go.** The TOML manifest becomes
  one loader among many, not the center of the API.
- **Backend-specific knobs live on the backend's config struct**, in a
  backend-named package under `backend/`. Zero QEMU vocabulary outside
  `backend/qemu`.

## Package layout

The repo root remains `package main` (the CLI, including the new
`virtle guest` daemon subcommand), so library packages are namespaced under
the module:

```
github.com/shazow/virtle                      package main — the virtle CLI
github.com/shazow/virtle/vm                   consumer-facing: Spec, Guest
github.com/shazow/virtle/backend              implementer contract: Backend, Instance, capability interfaces
github.com/shazow/virtle/backend/qemu         QEMU backend + its QGA-based vm.Guest (equivalent to today)
github.com/shazow/virtle/backend/firecracker  (future) example sibling backend
github.com/shazow/virtle/guest                virtle-native guest daemon + host-side client (implements vm.Guest)
github.com/shazow/virtle/manifest             TOML ⇄ (vm.Spec, backend.Backend) loader
github.com/shazow/virtle/units                typed scalars (units.MiB, units.Duration, ...)
internal/...                                  protocol clients (QMP, QGA wire), process supervision, etc.
```

Import direction is one-way: `backend` imports `vm` (for `Spec` and `Guest`
in signatures); `vm` never imports `backend`. Two structural consequences:

- There is no default backend anywhere in the core — `backend` cannot
  import `backend/qemu` without a cycle, so consumers always name their
  backend explicitly (`&qemu.Backend{}`). The layering question answers
  itself.
- `vm` cannot alias the contract types, so consumers of both import both
  packages — the same shape `database/sql` users live with.

## The API

### Consumer-facing types: `vm`

```go
package vm // github.com/shazow/virtle/vm

// Spec describes a virtual machine, independent of backend.
// The zero value plus a boot source is launchable; backends apply defaults.
type Spec struct {
	CPUs   int       // default: runtime.NumCPU
	Memory units.MiB // default: 2048
	Kernel *Kernel   // direct kernel boot (microVM style)
	Shares []Share   // host dirs shared into the guest (virtio-fs or similar)
	Disks  []Disk    // block devices / volume images
	Ports  []Forward // host↔guest port forwards
	Files  []File    // files placed in the guest before workload start
	Dir    string    // host working/state directory; default: derived tmp
}

type Kernel struct{ Path, Initrd, Cmdline string }
type Share struct {
	Tag, HostPath, GuestPath string
	ReadOnly                 bool
}
type Disk struct {
	Path, GuestPath, Format string
	Size                    units.MiB // created if absent
}
type Forward struct{ HostAddr, GuestAddr, Proto string } // Proto defaults to "tcp"
type File struct {
	GuestPath string
	Content   io.Reader
	Mode      fs.FileMode
}
```

```go
// Guest performs operations inside a running VM. Implemented by the
// host-side client in ./guest (virtle-native daemon) and by backend/qemu's
// QGA adapter. Shapes are os/exec- and io/fs-flavored, never protocol-
// flavored.
type Guest interface {
	Run(ctx context.Context, cmd *GuestCmd) (*GuestResult, error)
	Open(ctx context.Context, name string) (io.ReadCloser, error)
	Create(ctx context.Context, name string, mode fs.FileMode) (io.WriteCloser, error)
	Shutdown(ctx context.Context) error
	Close() error
}

type GuestCmd struct {
	Path  string
	Args  []string
	Env   []string
	Dir   string
	Stdin io.Reader
}
type GuestResult struct {
	ExitCode       int
	Stdout, Stderr []byte
}
```

Daemon-only features (streaming exec, file watching, richer process
control, ...) arrive later as `GuestWithX` extension interfaces on the
./guest client without touching `vm.Guest`.

### Implementer contract: `backend`

```go
package backend // github.com/shazow/virtle/backend

// Backend starts virtual machines. Implementations live under backend/
// (backend/qemu today; backend/firecracker, an in-process libkrun
// backend, ... later).
type Backend interface {
	Start(ctx context.Context, spec *vm.Spec) (Instance, error)
}

// Instance is a virtual machine started by a Backend. It deliberately says
// nothing about processes, sockets, or protocols, so exec'd (QEMU) and
// in-process (libkrun) backends satisfy it equally.
type Instance interface {
	Wait(ctx context.Context) error // blocks until the VM exits
	Kill() error                    // hard stop, always available

	// RemoteControl returns guest control for this instance, wired up by
	// the backend, or an error wrapping errors.ErrUnsupported when the VM
	// has no reachable guest agent. Most virtle functionality is built on
	// the expectation that this succeeds. Whether the backend wires guest
	// control eagerly at Start or lazily on first call is an implementation
	// detail behind the backend's constructor.
	RemoteControl() (vm.Guest, error)
}
```

Optional functionality is declared as standalone capability interfaces —
short `-er` names qualified by the package, so use sites read as one family
(`backend.Suspender`, `backend.MemoryResizer`) without a common prefix
baked into the type names. They deliberately do **not** embed `Backend`:
the caller asserting a capability already holds the `Backend`, standalone
interfaces compose freely (`interface { backend.Backend;
backend.Suspender }`), and this matches `driver.Pinger` / `http.Flusher`
precedent (the embedding variant, `fs.ReadDirFS`-style, buys nothing
here).

```go
// Suspender is implemented by backends that can save a running instance's
// state to disk and later restore it.
type Suspender interface {
	Suspend(ctx context.Context, inst Instance, stateDir string) error
	Resume(ctx context.Context, spec *vm.Spec, stateDir string) (Instance, error)
}

// MemoryResizer is implemented by backends that can grow or shrink a
// running instance's memory (e.g. virtio-balloon).
type MemoryResizer interface {
	ResizeMemory(ctx context.Context, inst Instance, bytes int64) error
}

// DeviceAttacher is implemented by backends that can attach and detach
// devices on a running instance.
type DeviceAttacher interface {
	Attach(ctx context.Context, inst Instance, dev any) error // dev: vm.Share, vm.Disk, vm.Forward
	Detach(ctx context.Context, inst Instance, dev any) error
}

// Shutdown stops an instance gracefully: guest shutdown via RemoteControl
// when available, falling back to Kill when the guest is unreachable or the
// context expires.
func Shutdown(ctx context.Context, inst Instance) error
```

Naming notes: `backend.Backend` stutters, but so does `driver.Driver`; the
stutter is accepted for greppability over `backend.Interface`
(`sort.Interface` precedent, a style the stdlib has moved away from). One
ergonomic caveat: callers cannot name a variable `backend`, so examples and
docs model `b, err := qemu.BackendWithQGA(...)`.

### The virtle guest daemon: `guest`

```go
package guest // github.com/shazow/virtle/guest

// Serve runs the virtle guest daemon inside a VM, speaking to the virtle
// host over the given listener (unix socket or vsock). The `virtle guest`
// CLI subcommand is a thin wrapper over this.
func Serve(ctx context.Context, l net.Listener) error

// Dial connects to a running virtle guest daemon from the host side.
// The returned Client implements vm.Guest.
func Dial(ctx context.Context, addr string) (*Client, error) // "vsock://cid:port", "unix://path"

type Client struct{ ... } // implements vm.Guest (+ future GuestWithX extensions)
```

Both halves live in one package so the protocol has a single home; the
daemon binary is just the CLI running `guest.Serve`.

Portability requirements (decided in PR #67 review): the guest daemon ships
inside the regular `virtle` binary as `virtle guest`, injected into guest
images as-is, so release builds must be fully static —
`CGO_ENABLED=0` (verified: no current dependency needs cgo; ~8.7 MB with
`-ldflags "-s -w"`), enforced by a CI check on the release artifact. To
keep that true and leave room for a minimal guest-only binary later
(~3 MB via a separate `cmd/virtle-guest` main, no build tags needed),
`./guest`'s dependency budget is stdlib plus `golang.org/x/sys` (vsock)
only.

### Backend: `backend/qemu`

```go
package qemu // github.com/shazow/virtle/backend/qemu

// Config holds the QEMU-only knobs. The zero value works.
type Config struct {
	Binary    string   // default: qemu-system-<arch>
	Machine   string   // default: microvm/q35 per arch
	ExtraArgs []string // passthrough
	Console   Console
	Seccomp   bool
	// ... QMP/QGA socket placement, virtiofsd path, etc.
}

// Constructors select the RemoteControl implementation (if any); whether
// guest control is wired eagerly or lazily is an implementation detail
// behind them, so variants can be tried without API changes.

// BackendWithQGA returns a QEMU backend whose instances' RemoteControl
// speaks the QEMU Guest Agent — equivalent to today's codebase.
func BackendWithQGA(cfg Config) (backend.Backend, error)

// BackendWithGuest (future) returns a QEMU backend whose instances'
// RemoteControl speaks the virtle-native guest daemon.
func BackendWithGuest(cfg Config) (backend.Backend, error)
```

The returned backends also implement `backend.Suspender` (QMP migration
save/restore), `backend.MemoryResizer` (virtio-balloon), and
`backend.DeviceAttacher` (QMP device_add/del). virtiofsd helper processes
are an implementation detail inside the backend. `qmpclient`, `qga`, and
`qmpwire` stay internal; nothing in `backend/qemu`'s exported surface names
QMP beyond the `BackendWithQGA` constructor that opts into it.

### Config files: `manifest`

```go
package manifest

// Load reads a virtle manifest (TOML) and lowers it to a neutral Spec plus
// the backend it configures.
func Load(r io.Reader) (*vm.Spec, backend.Backend, error)
```

The `[qemu]` TOML table maps onto `qemu.Backend` fields. CLI-only concerns —
host `[run]` helper commands, SSH session handling, notifications, the
control RPC socket — stay in the CLI and higher layers: they are session
orchestration, not VM description (see D5).

## Usage examples

**1. Minimal virtle — boot, run a command, tear down:**

```go
spec := &vm.Spec{
	Kernel: &vm.Kernel{Path: "vmlinuz", Initrd: "initrd.img"},
	Shares: []vm.Share{{Tag: "src", HostPath: ".", GuestPath: "/workspace"}},
	Memory: 2048, // units.MiB
}
b, err := qemu.BackendWithQGA(qemu.Config{})
if err != nil {
	log.Fatal(err)
}
inst, err := b.Start(ctx, spec)
if err != nil {
	log.Fatal(err)
}
defer backend.Shutdown(ctx, inst)

g, err := inst.RemoteControl()
if err != nil {
	log.Fatal(err) // this VM has no guest agent
}
out, err := g.Run(ctx, &vm.GuestCmd{Path: "make", Dir: "/workspace"})
fmt.Printf("exit=%d\n%s", out.ExitCode, out.Stdout)
```

**2. Swapping the backend — the abstraction paying off:**

```go
b, err := qemu.BackendWithQGA(qemu.Config{Machine: "microvm"})
if useFirecracker {
	b, err = firecracker.New(firecracker.Config{}) // hypothetical sibling
}
inst, err := b.Start(ctx, spec)
```

Nothing after `Start` changes: `Instance` never exposed a process or a
socket, so an in-process libkrun backend satisfies it exactly as an exec'd
QEMU does.

**3. Optional functionality — suspend if the backend supports it:**

```go
if s, ok := b.(backend.Suspender); ok {
	if err := s.Suspend(ctx, inst, stateDir); err != nil {
		return err
	}
	// later, possibly in another process:
	inst, err = s.Resume(ctx, spec, stateDir)
} else {
	err = backend.Shutdown(ctx, inst) // graceful fallback
}
```

**4. Manifest-driven — what the CLI does:**

```go
spec, b, err := manifest.Load(f)
inst, err := b.Start(ctx, spec)
```

**5. Inside the guest — the daemon the host talks to:**

```go
// `virtle guest` subcommand, roughly:
l, err := vsock.Listen(guest.DefaultPort)
err = guest.Serve(ctx, l)
```

What the examples validate: the minimal case is a few lines to a running VM
with no orchestrator type to learn — `Backend.Start` → `Instance` →
`RemoteControl` is the whole model; guest operations read like `os/exec` +
`io/fs`; choosing a backend is one assignment; optional functionality is a
package-qualified type assertion, exactly as in `database/sql/driver`.

## Decision points

Resolved by this revision:

- Root stays `package main`; the library splits `database/sql`-style into
  consumer-facing `vm` (`Spec`, `Guest`) and the implementer contract
  `backend` (`Backend`, `Instance`, capabilities), with implementations
  under `backend/` (`backend/qemu`, ...).
- Optional functionality uses standalone package-qualified `-er` capability
  interfaces (`backend.Suspender` covering both Suspend and Resume,
  `backend.MemoryResizer`, `backend.DeviceAttacher`) — no `Backend`
  embedding, composed where needed.
- The core interface is named `backend.Backend` (accepting the
  `driver.Driver`-style stutter over `backend.Interface`).
- No default backend in core — enforced structurally by the import
  direction.
- Guest control is obtained from the instance via
  `Instance.RemoteControl() (vm.Guest, error)`, not from a dialer option;
  the virtle-native daemon lives in `./guest` with host-side constructors
  returning `vm.Guest` implementations.
- **D6 — sizes** (PR #67 review): lean on the type system — `./units`
  becomes public and `vm.Spec` uses typed scalars (`units.MiB`,
  `units.Duration`, more as needed) instead of plumbed `int64`s whose
  meaning can be misread.
- **D7 — RemoteControl selection** (PR #67 review): backend constructors
  select the guest-control implementation — `qemu.BackendWithQGA(...)
  (backend.Backend, error)` first, a `qemu.BackendWithGuest` equivalent
  later for the virtle-native daemon. Whether wiring is lazy or eager is an
  implementation detail behind the constructor so variants can be tried
  without API changes.

Still open:

- **D5 — scope of Spec.**
  (a) *Recommended:* VM description only. Host helper processes, SSH
  sessions, and the RPC control socket are higher-layer/CLI concerns.
  (b) Include host `Runs` in Spec for manifest parity — fatter core,
  simpler CLI.

- **D8 — where instance-scoped optional ops live.**
  This design puts Suspend/ResizeMemory/Attach on backend-level capability
  interfaces taking the `Instance` as an argument (capability is a property
  of the backend, matching the sql/driver framing). The alternative is
  instance-level interfaces asserted on the instance itself. Backend-level
  keeps discovery in one place; instance-level reads slightly more
  naturally at call sites. Recommend backend-level as specified, revisit
  only if call sites get awkward.

- **D9 — tty / interactive sessions** (under discussion, PR #67 thread).
  Recommended direction: standardize the *session type* in core — `vm.Term`
  (`io.ReadWriteCloser` + `Resize` + `Wait`) with `vm.TermOptions` — rather
  than a transport-driver registry, and let each source expose it where its
  ownership naturally lives: the serial/chardev console as a
  `backend.ConsoleProvider` capability on instances (backend resource, the
  no-daemon debug path), an interactive terminal as a `GuestWithTerminal`
  extension on the guest-daemon client (the primary programmatic path once
  the daemon lands), and SSH staying at the CLI layer as today
  (exec the user's ssh client; its value is their config/agent/tooling),
  with an optional pure-Go `sshterm` transport implementing `vm.Term` only
  if a library consumer needs programmatic SSH ttys.

## Appendix: mapping to existing internals

The new API is a re-skin, not a rewrite — each piece has an existing
implementation to be adapted:

| New API piece | Existing code that becomes its implementation |
|---|---|
| `qemu.Backend.Start` | `internal/manager/qemu.go` lowering + `internal/manager/launch` (`BuildPlan`, `AcquireCID`, socket waits) + `internal/executor` supervision |
| `backend.Instance` | `internal/executor.Process` (later, libkrun: a cgo handle) |
| `backend/qemu`'s QGA `vm.Guest` | `internal/qga` client behind the new shapes (`guest-exec` → `Run`, base64 file chunks → `Open`/`Create`) |
| `guest` daemon + client | new code; protocol can grow from `internal/manager/control`'s JSON-RPC framing |
| `backend.Suspender` | `internal/qmpclient` migration save/restore + `launch.SuspendState` |
| `backend.MemoryResizer` | `internal/balloon` controller / QMP path |
| `backend.DeviceAttacher` | `internal/hotplug` + its QMP adapter |
| `manifest.Load` | `internal/manifest` decode + resolve, retargeted at (Spec, Backend) |

## Non-goals

This document does not freeze the current control-socket wire format or the
suspend-state on-disk format; those are revisited during migration. It also
does not schedule the migration itself — the staging of moving the current
CLI onto this API is follow-up work once the API shape is agreed.
