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
   but QGA will ultimately be replaced by a virtle-native guest daemon. Guest
   operations must not leak QGA protocol shapes, and the guest transport must
   be separable from the backend — a native daemon over vsock should work
   identically under any backend.

## Design principles

- **stdlib-shaped.** The core follows the `http.Client` / `http.Transport`
  pattern: a concrete, user-facing orchestrator over a *small* backend
  interface. Config is plain structs with usable zero values, not functional
  options. Blocking calls take a `context.Context` first. Data flows through
  `io` interfaces. Errors use sentinels plus `errors.Is`, and absent
  capabilities return `errors.ErrUnsupported`.
- **Interfaces are discovered, not designed.** The backend and guest
  interfaces are cut to the minimum the orchestrator provably needs today.
  Everything else is an optional extension interface (the `io.ReaderAt` /
  `http.Pusher` pattern), so adding capabilities never breaks implementers.
- **The Spec is neutral and constructable in Go.** The TOML manifest becomes
  one loader among many, not the center of the API.
- **Backend-specific knobs live on the backend's config struct**, in a
  backend-named package. Zero QEMU vocabulary outside `qemu/`.

## Package layout

```
github.com/shazow/virtle             package virtle — Spec, Start, VM, Guest, capabilities
github.com/shazow/virtle/qemu        package qemu — Backend implementation + QEMU-only knobs
github.com/shazow/virtle/manifest    package manifest — TOML ⇄ (Spec, Backend) loader
github.com/shazow/virtle/cmd/virtle  the CLI (moves out of the repo root)
internal/...                         protocol clients (QMP, QGA wire), process
                                     supervision, and everything else
```

A root importable `virtle` package requires moving the CLI to `cmd/virtle`,
which changes the `go install` path — see decision D1.

## The API

### Core: `virtle`

```go
// Spec describes a virtual machine, independent of backend.
// The zero value plus Root or Kernel is launchable; Start applies defaults.
type Spec struct {
	CPUs   int       // default: runtime.NumCPU
	Memory int64     // bytes; default 2 GiB (see D6)
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
	Size                    int64 // created if absent
}
type Forward struct{ HostAddr, GuestAddr, Proto string } // Proto defaults to "tcp"
type File struct {
	GuestPath string
	Content   io.Reader
	Mode      fs.FileMode
}
```

```go
// Backend turns a Spec into a running machine. qemu.Backend implements this
// by exec'ing QEMU; a krun.Backend would run the VM in-process.
type Backend interface {
	Start(ctx context.Context, spec *Spec) (Machine, error)
}

// Machine is the minimal running-VM contract a Backend must provide.
// It deliberately says nothing about processes, sockets, or protocols.
type Machine interface {
	Wait(ctx context.Context) error // blocks until the VM exits
	Kill() error                    // hard stop, always available
	GuestAddr() GuestAddr           // where a guest agent can be reached
}

// GuestAddr locates the guest agent transport without naming a protocol.
type GuestAddr struct {
	VSockCID int    // nonzero when the guest is reachable over vsock
	Socket   string // host unix socket path, when proxied by the backend
}

// Optional backend capabilities, asserted at runtime.
type Suspender interface {
	Suspend(ctx context.Context, stateDir string) error
}
type Resumer interface {
	Resume(ctx context.Context, spec *Spec, stateDir string) (Machine, error)
}
type MemoryResizer interface {
	ResizeMemory(ctx context.Context, bytes int64) error
}
type DeviceAttacher interface {
	Attach(ctx context.Context, dev any) error // dev: Share, Disk, Forward
	Detach(ctx context.Context, dev any) error
}
```

```go
// VM is the concrete orchestrator (the http.Client analog): it wraps a
// Machine with guest wiring, readiness, and graceful shutdown.
func Start(ctx context.Context, spec *Spec, opts *Options) (*VM, error)

type Options struct {
	Backend Backend     // default: &qemu.Backend{} (see D2)
	Guest   GuestDialer // default: backend-appropriate agent dialer
	Logger  *slog.Logger
}

func (vm *VM) Guest(ctx context.Context) (Guest, error) // dials + caches; waits for readiness
func (vm *VM) Wait(ctx context.Context) error
func (vm *VM) Close() error                    // graceful: guest shutdown if reachable → Kill fallback
func (vm *VM) Suspend(ctx context.Context) error // errors.ErrUnsupported if the backend can't
```

### Guest operations (QGA-free)

```go
// Guest performs operations inside a running VM. Implemented today by an
// internal QGA adapter; later by the virtle-native guest daemon. Shapes are
// os/exec- and io/fs-flavored, never QGA-flavored.
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

// GuestDialer connects to whatever agent serves GuestAddr.
type GuestDialer interface {
	DialGuest(ctx context.Context, addr GuestAddr) (Guest, error)
}
```

The native-daemon migration is then invisible to users: a `guestd.Dialer`
(vsock transport, richer operations exposed via extension interfaces)
replaces the QGA dialer in `Options.Guest`, and user code is untouched.

### Backend: `qemu`

```go
package qemu

// Backend implements virtle.Backend by exec'ing QEMU.
// The zero value works; fields are the QEMU-only knobs.
type Backend struct {
	Binary    string   // default: qemu-system-<arch>
	Machine   string   // default: microvm/q35 per arch
	ExtraArgs []string // passthrough
	Console   Console
	Seccomp   bool
	// ... QMP/QGA socket placement, virtiofsd path, etc.
}

func (b *Backend) Start(ctx context.Context, spec *virtle.Spec) (virtle.Machine, error)
```

The Machine returned by `qemu.Backend` also implements `Suspender` and
`Resumer` (QMP migration save/restore), `MemoryResizer` (virtio-balloon),
and `DeviceAttacher` (QMP hotplug). virtiofsd helper processes are an
implementation detail *inside* the backend. `qmpclient`, `qga`, and
`qmpwire` stay internal; nothing in `qemu`'s exported surface names QMP.

### Config files: `manifest`

```go
package manifest

// Load reads a virtle manifest (TOML) and lowers it to a neutral Spec plus
// the backend it configures.
func Load(r io.Reader) (*virtle.Spec, virtle.Backend, error)
```

The `[qemu]` TOML table maps onto `qemu.Backend` fields. CLI-only concerns —
host `[run]` helper commands, SSH session handling, notifications, the
control RPC socket — stay in `cmd/virtle` and higher layers: they are
session orchestration, not VM description (see D5).

## Usage examples

**1. Minimal virtle — boot, run a command, tear down:**

```go
spec := &virtle.Spec{
	Kernel: &virtle.Kernel{Path: "vmlinuz", Initrd: "initrd.img"},
	Shares: []virtle.Share{{Tag: "src", HostPath: ".", GuestPath: "/workspace"}},
	Memory: 2 << 30,
}
vm, err := virtle.Start(ctx, spec, nil) // nil opts → default QEMU backend
if err != nil {
	log.Fatal(err)
}
defer vm.Close()

g, err := vm.Guest(ctx) // waits for agent readiness
out, err := g.Run(ctx, &virtle.GuestCmd{Path: "make", Dir: "/workspace"})
fmt.Printf("exit=%d\n%s", out.ExitCode, out.Stdout)
```

**2. Swapping the backend — the abstraction paying off:**

```go
var b virtle.Backend = &qemu.Backend{Machine: "microvm"}
if useKrun {
	b = &krun.Backend{} // hypothetical future in-process backend
}
vm, err := virtle.Start(ctx, spec, &virtle.Options{Backend: b})
```

Nothing after `Start` changes: `Machine` never exposed a process or a
socket, so an in-process libkrun backend satisfies it exactly as an exec'd
QEMU does.

**3. Capabilities — suspend if supported:**

```go
if err := vm.Suspend(ctx); errors.Is(err, errors.ErrUnsupported) {
	log.Println("backend cannot suspend; shutting down instead")
	err = vm.Close()
}
```

**4. Manifest-driven — what the CLI does:**

```go
spec, backend, err := manifest.Load(f)
vm, err := virtle.Start(ctx, spec, &virtle.Options{Backend: backend})
```

What the examples validate: zero-value defaults make the minimal case a few
lines to a running VM; guest operations read like `os/exec` + `io/fs`;
choosing a backend is one assignment; capability degradation is a
stdlib-idiomatic `errors.Is`.

## Decision points

- **D1 — root package vs subpackage.**
  (a) *Recommended:* root `package virtle` with the CLI moved to
  `cmd/virtle`. Best import ergonomics (`virtle.Start`), at the cost of
  breaking `go install github.com/shazow/virtle@latest`.
  (b) Keep `package main` at the root and put the core API in `virtle/vm`
  (`vm.Start`, `vm.Spec`). No install-path break, slightly worse name.

- **D2 — default backend in core?**
  (a) *Recommended:* `Options.Backend == nil` → `&qemu.Backend{}`, which
  makes core import qemu (pragmatic; mirrors `http.DefaultTransport`).
  (b) Purist: core imports no backend; `Start` errors on a nil Backend and
  `qemu.Start(ctx, spec)` exists as a convenience. Cleaner layering, one
  more line for everyone.

- **D3 — Guest as interface vs concrete struct.**
  (a) *Recommended:* small interface plus extension interfaces for
  daemon-only features. Matches the capability pattern; easy fakes.
  (b) Concrete `*Guest` over an unexported connection — easier to grow
  methods without breaking third-party fakes, harder to substitute
  wholesale.

- **D4 — guest transport ownership.**
  (a) *Recommended:* orthogonal `GuestDialer` in `Options` plus
  `Machine.GuestAddr()`. The native-daemon swap becomes a config change and
  backends stay agent-agnostic.
  (b) Backend-owned (`Machine.Guest()`): simpler wiring, but couples every
  backend to every agent transition.

- **D5 — scope of Spec.**
  (a) *Recommended:* VM description only. Host helper processes, SSH
  sessions, and the RPC control socket are higher-layer/CLI concerns.
  (b) Include host `Runs` in Spec for manifest parity — fatter core,
  simpler CLI.

- **D6 — sizes.**
  (a) `int64` bytes everywhere (stdlib-flavored, recommended).
  (b) Keep `units.MiB` / `units.Duration` typed scalars (nicer TOML
  round-trip, already exists).

## Appendix: mapping to existing internals

The new API is a re-skin, not a rewrite — each piece has an existing
implementation to be adapted:

| New API piece | Existing code that becomes its implementation |
|---|---|
| `qemu.Backend.Start` | `internal/manager/qemu.go` lowering + `internal/manager/launch` (`BuildPlan`, `AcquireCID`, socket waits) + `internal/executor` supervision |
| `virtle.Machine` | `internal/executor.Process` (later, libkrun: a cgo handle) |
| QGA `GuestDialer` / `Guest` | `internal/qga` client behind the new shapes (`guest-exec` → `Run`, base64 file chunks → `Open`/`Create`) |
| `Suspender` / `Resumer` | `internal/qmpclient` migration save/restore + `launch.SuspendState` |
| `MemoryResizer` | `internal/balloon` controller / QMP path |
| `DeviceAttacher` | `internal/hotplug` + its QMP adapter |
| `manifest.Load` | `internal/manifest` decode + resolve, retargeted at (Spec, Backend) |

## Non-goals

This document does not freeze the current control-socket wire format or the
suspend-state on-disk format; those are revisited during migration. It also
does not schedule the migration itself — the staging of moving the current
CLI onto this API is follow-up work once the API shape is agreed.
