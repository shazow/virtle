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

- **stdlib-shaped.** The backend seam follows the `database/sql/driver`
  design: a small required interface (`vm.Backend`, `vm.Instance`) that
  implementations satisfy, with optional functionality declared through
  `XWithY` extension interfaces (`vm.BackendWithSuspend`, ...) discovered by
  type assertion — the way `driver.Conn` implementations opt into
  `driver.ConnBeginTx`. Config is plain structs with usable zero values, not
  functional options. Blocking calls take a `context.Context` first. Data
  flows through `io` interfaces. Errors use sentinels plus `errors.Is`, and
  absent capabilities return `errors.ErrUnsupported`.
- **Interfaces are discovered, not designed.** `vm.Backend` and `vm.Guest`
  are cut to the minimum provably needed today; everything else is an
  extension interface, so adding capabilities never breaks implementers.
- **The Spec is neutral and constructable in Go.** The TOML manifest becomes
  one loader among many, not the center of the API.
- **Backend-specific knobs live on the backend's config struct**, in a
  backend-named package under `vm/`. Zero QEMU vocabulary outside `vm/qemu`.

## Package layout

The repo root remains `package main` (the CLI, including the new
`virtle guest` daemon subcommand), so library packages are namespaced under
the module:

```
github.com/shazow/virtle                 package main — the virtle CLI
github.com/shazow/virtle/vm              core: Spec, Backend, Instance, Guest, capability interfaces
github.com/shazow/virtle/vm/qemu         QEMU backend + its QGA-based vm.Guest (equivalent to today)
github.com/shazow/virtle/vm/firecracker  (future) example sibling backend
github.com/shazow/virtle/guest           virtle-native guest daemon + host-side client (implements vm.Guest)
github.com/shazow/virtle/manifest        TOML ⇄ (vm.Spec, vm.Backend) loader
internal/...                             protocol clients (QMP, QGA wire), process supervision, etc.
```

A structural consequence worth noting: `vm` cannot import `vm/qemu` without
an import cycle, so the core has no default backend — consumers always name
their backend explicitly (`&qemu.Backend{}`). The layering question answers
itself.

## The API

### Core: `vm`

```go
package vm // github.com/shazow/virtle/vm

// Spec describes a virtual machine, independent of backend.
// The zero value plus a boot source is launchable; backends apply defaults.
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
// Backend starts virtual machines. Implementations live under vm/
// (vm/qemu today; vm/firecracker, an in-process libkrun backend, ... later).
type Backend interface {
	Start(ctx context.Context, spec *Spec) (Instance, error)
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
	// the expectation that this succeeds.
	RemoteControl() (Guest, error)
}

// Optional backend functionality, discovered by type assertion
// (the database/sql/driver pattern):

// BackendWithSuspend is implemented by backends that can save a running
// instance's state to disk and later restore it.
type BackendWithSuspend interface {
	Backend
	Suspend(ctx context.Context, inst Instance, stateDir string) error
	Resume(ctx context.Context, spec *Spec, stateDir string) (Instance, error)
}

// BackendWithMemoryResize is implemented by backends that can grow or
// shrink a running instance's memory (e.g. virtio-balloon).
type BackendWithMemoryResize interface {
	Backend
	ResizeMemory(ctx context.Context, inst Instance, bytes int64) error
}

// BackendWithHotplug is implemented by backends that can attach and detach
// devices on a running instance.
type BackendWithHotplug interface {
	Backend
	Attach(ctx context.Context, inst Instance, dev any) error // dev: Share, Disk, Forward
	Detach(ctx context.Context, inst Instance, dev any) error
}
```

```go
// Shutdown stops an instance gracefully: guest shutdown via RemoteControl
// when available, falling back to Kill when the guest is unreachable or the
// context expires.
func Shutdown(ctx context.Context, inst Instance) error
```

### Guest control: `vm.Guest`

```go
// Guest performs operations inside a running VM. Implemented by the
// host-side client in ./guest (virtle-native daemon) and by vm/qemu's
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

### Backend: `vm/qemu`

```go
package qemu // github.com/shazow/virtle/vm/qemu

// Backend implements vm.Backend by exec'ing QEMU.
// The zero value works; fields are the QEMU-only knobs.
type Backend struct {
	Binary    string   // default: qemu-system-<arch>
	Machine   string   // default: microvm/q35 per arch
	ExtraArgs []string // passthrough
	Console   Console
	Seccomp   bool
	// ... QMP/QGA socket placement, virtiofsd path, etc.
}

func (b *Backend) Start(ctx context.Context, spec *vm.Spec) (vm.Instance, error)
```

`qemu.Backend` also implements `vm.BackendWithSuspend` (QMP migration
save/restore), `vm.BackendWithMemoryResize` (virtio-balloon), and
`vm.BackendWithHotplug` (QMP device_add/del). Its instances' `RemoteControl`
returns the virtle guest daemon client when the spec boots one, and
otherwise falls back to the package's QGA-based `vm.Guest` implementation —
equivalent to today's codebase (see D7). virtiofsd helper processes are an
implementation detail inside the backend. `qmpclient`, `qga`, and `qmpwire`
stay internal; nothing in `vm/qemu`'s exported surface names QMP.

### Config files: `manifest`

```go
package manifest

// Load reads a virtle manifest (TOML) and lowers it to a neutral Spec plus
// the backend it configures.
func Load(r io.Reader) (*vm.Spec, vm.Backend, error)
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
	Memory: 2 << 30,
}
backend := &qemu.Backend{}
inst, err := backend.Start(ctx, spec)
if err != nil {
	log.Fatal(err)
}
defer vm.Shutdown(ctx, inst)

g, err := inst.RemoteControl()
if err != nil {
	log.Fatal(err) // this VM has no guest agent
}
out, err := g.Run(ctx, &vm.GuestCmd{Path: "make", Dir: "/workspace"})
fmt.Printf("exit=%d\n%s", out.ExitCode, out.Stdout)
```

**2. Swapping the backend — the abstraction paying off:**

```go
var backend vm.Backend = &qemu.Backend{Machine: "microvm"}
if useFirecracker {
	backend = &firecracker.Backend{} // hypothetical sibling
}
inst, err := backend.Start(ctx, spec)
```

Nothing after `Start` changes: `Instance` never exposed a process or a
socket, so an in-process libkrun backend satisfies it exactly as an exec'd
QEMU does.

**3. Optional functionality — suspend if the backend supports it:**

```go
if s, ok := backend.(vm.BackendWithSuspend); ok {
	if err := s.Suspend(ctx, inst, stateDir); err != nil {
		return err
	}
	// later, possibly in another process:
	inst, err = s.Resume(ctx, spec, stateDir)
} else {
	err = vm.Shutdown(ctx, inst) // graceful fallback
}
```

**4. Manifest-driven — what the CLI does:**

```go
spec, backend, err := manifest.Load(f)
inst, err := backend.Start(ctx, spec)
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
type assertion, exactly as in `database/sql/driver`.

## Decision points

Resolved by this revision:

- Root stays `package main`; the core library is `vm` (`vm.Spec`,
  `vm.Backend`, `vm.Instance`).
- Optional functionality uses `XWithY` interfaces on the backend
  (`vm.BackendWithSuspend` covering both Suspend and Resume, etc.).
- No default backend in core — enforced structurally by the `vm/qemu`
  import direction.
- Guest control is obtained from the instance via
  `Instance.RemoteControl() (vm.Guest, error)`, not from a dialer option;
  the virtle-native daemon lives in `./guest` with host-side constructors
  returning `vm.Guest` implementations.

Still open:

- **D5 — scope of Spec.**
  (a) *Recommended:* VM description only. Host helper processes, SSH
  sessions, and the RPC control socket are higher-layer/CLI concerns.
  (b) Include host `Runs` in Spec for manifest parity — fatter core,
  simpler CLI.

- **D6 — sizes.**
  (a) `int64` bytes everywhere (stdlib-flavored, recommended).
  (b) Keep `units.MiB` / `units.Duration` typed scalars (nicer TOML
  round-trip, already exists).

- **D7 — how RemoteControl picks its implementation.**
  `RemoteControl()` takes no context, implying the backend wires guest
  control during `Start` (dial/retry happens there, under Start's ctx).
  (a) *Recommended:* a `qemu.Backend` field selects the agent
  (`GuestControl: guestd | qga | none`), defaulting to the virtle daemon
  with QGA fallback during the transition.
  (b) Probe at start: try the virtle daemon port, fall back to QGA if it
  doesn't answer — zero config, but adds boot-time latency and
  nondeterminism.
  A lazy variant (`RemoteControl(ctx)` dialing on first use) is possible if
  wiring at Start proves too eager.

- **D8 — where instance-scoped optional ops live.**
  This design puts Suspend/ResizeMemory/Attach on backend-level `XWithY`
  interfaces taking the `Instance` as an argument (capability is a property
  of the backend, matching the sql/driver framing). The alternative is
  instance-level interfaces (`vm.InstanceWithSuspend`, asserted on the
  instance). Backend-level keeps discovery in one place; instance-level
  reads slightly more naturally at call sites. Recommend backend-level as
  specified, revisit only if call sites get awkward.

## Appendix: mapping to existing internals

The new API is a re-skin, not a rewrite — each piece has an existing
implementation to be adapted:

| New API piece | Existing code that becomes its implementation |
|---|---|
| `qemu.Backend.Start` | `internal/manager/qemu.go` lowering + `internal/manager/launch` (`BuildPlan`, `AcquireCID`, socket waits) + `internal/executor` supervision |
| `vm.Instance` | `internal/executor.Process` (later, libkrun: a cgo handle) |
| `vm/qemu`'s QGA `vm.Guest` | `internal/qga` client behind the new shapes (`guest-exec` → `Run`, base64 file chunks → `Open`/`Create`) |
| `guest` daemon + client | new code; protocol can grow from `internal/manager/control`'s JSON-RPC framing |
| `vm.BackendWithSuspend` | `internal/qmpclient` migration save/restore + `launch.SuspendState` |
| `vm.BackendWithMemoryResize` | `internal/balloon` controller / QMP path |
| `vm.BackendWithHotplug` | `internal/hotplug` + its QMP adapter |
| `manifest.Load` | `internal/manifest` decode + resolve, retargeted at (Spec, Backend) |

## Non-goals

This document does not freeze the current control-socket wire format or the
suspend-state on-disk format; those are revisited during migration. It also
does not schedule the migration itself — the staging of moving the current
CLI onto this API is follow-up work once the API shape is agreed.
