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
github.com/shazow/virtle/units                typed scalars (units.Bytes + size constants, units.Duration, ...)
internal/...                                  protocol clients (QMP, QGA wire), process supervision, etc.
```

Import direction is one-way: `backend` imports `vm` (for `Spec` and `Guest`
in signatures); `vm` never imports `backend`. Two structural consequences:

- There is no default backend anywhere in the core — `backend` cannot
  import `backend/qemu` without a cycle, so consumers always name their
  backend explicitly (`qemu.BackendWithQGA(...)`). The layering question
  answers itself.
- `vm` cannot alias the contract types, so consumers of both import both
  packages — the same shape `database/sql` users live with.

## The API

### Consumer-facing types: `vm`

```go
package vm // github.com/shazow/virtle/vm

// Spec describes a virtual machine, independent of backend. The zero value
// plus a boot source is launchable; backends apply defaults. A Spec holds
// no live resources and is reusable across Start and Resume calls, with
// one caveat: Files content readers are consumed by Start (see File).
type Spec struct {
	CPUs   int         // default: runtime.NumCPU
	Memory units.Bytes // default: 2048 * units.Mebibyte
	Kernel Kernel      // direct kernel boot (microVM style); zero value: none
	Shares []Share     // host dirs shared into the guest (virtio-fs or similar)
	Disks  []Disk      // block devices / volume images
	Ports  []Forward   // host↔guest port forwards
	Files  []File      // small files placed in the guest before workload start
	Dir    string      // host working/state directory; default: derived tmp
}

type Kernel struct{ Path, Initrd, Cmdline string }
type Share struct {
	Tag, HostPath, GuestPath string
	ReadOnly                 bool
}
type Disk struct {
	Path, GuestPath, Format string
	Size                    units.Bytes // created if absent
}
type Forward struct{ HostAddr, GuestAddr, Proto string } // Proto defaults to "tcp"

// File is a small file placed in the guest before the workload starts;
// large trees go through GuestWithCopy after boot. Content is consumed by
// Start — refresh it (e.g. a fresh bytes.NewReader) before reusing the
// Spec.
type File struct {
	GuestPath string
	Content   io.Reader
	Mode      fs.FileMode
}

// Device is a device description that can be attached to a running
// instance: Share, Disk, or Forward. It is sealed (unexported method) so
// backend.DeviceAttacher stays typed rather than accepting `any`.
type Device interface{ device() }
```

```go
// Guest performs operations inside a running VM. Implemented by the
// host-side client in ./guest (virtle-native daemon) and by backend/qemu's
// QGA adapter. Shapes are os/exec- and io/fs-flavored, never protocol-
// flavored. Implementations must be safe for concurrent use.
type Guest interface {
	// Run executes a command to completion with buffered output (the
	// exec.Cmd.Output analog); streaming exec arrives as an extension.
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

Daemon-only features (file-tree copy, streaming exec, file watching, ...)
are `GuestWithX` extension interfaces: declared here in `vm` (they use only
stdlib and `vm` types, so consumers assert them without importing `guest`),
implemented by the ./guest client, never added to `vm.Guest` itself.

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
	ResizeMemory(ctx context.Context, inst Instance, size units.Bytes) error
}

// DeviceAttacher is implemented by backends that can attach and detach
// devices on a running instance. vm.Device is the sealed union of
// vm.Share, vm.Disk, and vm.Forward — typed, not `any`.
type DeviceAttacher interface {
	Attach(ctx context.Context, inst Instance, dev vm.Device) error
	Detach(ctx context.Context, inst Instance, dev vm.Device) error
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

Protocol requirements (PR #67 review): the host↔guest transport is
**binary-safe, concurrent, and streaming** — no base64 payload encoding
(exactly the QGA limitation this daemon replaces), multiple requests in
flight on one connection, and bulk data as streams rather than chunked
request/response rounds. Bulk directory provisioning is the motivating
case: expressed as a streamed tar archive (stdlib `archive/tar`, optionally
wrapped in `compress/gzip`), surfaced as a `GuestWithX` extension on the
client:

```go
// In vm: GuestWithCopy streams file trees between host and guest as tar
// archives. Both directions speak the same shape (the Docker
// CopyToContainer/CopyFromContainer model), so a guest→guest copy is a
// direct pipe with no host filesystem and no buffering, and transforms
// compose as ordinary io.Reader middleware.
type GuestWithCopy interface {
	CopyToGuest(ctx context.Context, guestPath string, archive io.Reader, opts CopyOptions) error
	CopyFromGuest(ctx context.Context, guestPath string) (io.ReadCloser, error)
}

// ArchiveFS adapts the common host case — "copy this directory" — to the
// stream API: it returns a reader that lazily produces a tar stream of
// fsys as it is read, so nothing is buffered. Callers pass os.DirFS(path),
// an embed.FS, or a fstest.MapFS. Generation errors surface from Read.
//
//	err := g.CopyToGuest(ctx, "/workspace", vm.ArchiveFS(os.DirFS(src)), opts)
func ArchiveFS(fsys fs.FS) io.ReadCloser

// CopyOptions carries the options prior art shows are necessary for safe
// usage; nice-to-haves (preserve-times, mode masks, exclusions) wait until
// a consumer needs them. The zero value is the safe default.
type CopyOptions struct {
	// Overwrite replaces existing files instead of failing with an error
	// satisfying errors.Is(err, fs.ErrExist) — the os.CopyFS default.
	Overwrite bool

	// UID/GID override ownership of created entries; nil keeps whatever
	// the archive recorded (ArchiveFS records none, since host uids are
	// meaningless in-guest). Pointers are load-bearing: 0 (root) is a
	// valid value, so nil must be distinguishable from it.
	UID, GID *int
}
```

One safety rule is an invariant, not an option: extraction rejects entries
and symlinks that escape the target root (the zip-slip / `docker cp`
CVE-2018-15664 class). The guest daemon extracts as root, so this is
enforced on the guest side with no opt-out.

Guest→guest then needs no special API — it is the two halves piped
together, optionally through a tar-rewriting `io.Reader` middleware:

```go
rc, err := src.CopyFromGuest(ctx, "/data")
defer rc.Close()
err = dst.CopyToGuest(ctx, "/data", rc, vm.CopyOptions{})
```

The wire framing that delivers this is an implementation detail of the
protocol, with two candidate shapes: length-prefixed JSON control frames
with dedicated binary stream channels, or — if the daemon embeds an sshd
(D9) — reusing SSH's channel multiplexing for RPC and streams alike, one
mux instead of two. Decided during implementation; the API above doesn't
change either way. Whatever the framing, the protocol opens with a version
handshake: the daemon is baked into guest images and will routinely be
older or newer than the host virtle talking to it, so skew must fail (or
degrade) explicitly, never silently.

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
	Logger    *slog.Logger // default: discard; replaces today's package-global SetLogger
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

The `[qemu]` TOML table maps onto `qemu.Config` fields. CLI-only concerns —
host `[run]` helper commands, SSH session handling, notifications, the
control RPC socket — stay in the CLI and higher layers: they are session
orchestration, not VM description (see D5).

## Usage examples

**1. Minimal virtle — boot, run a command, tear down:**

```go
spec := &vm.Spec{
	Kernel: vm.Kernel{Path: "vmlinuz", Initrd: "initrd.img"},
	Shares: []vm.Share{{Tag: "src", HostPath: ".", GuestPath: "/workspace"}},
	Memory: 2048 * units.Mebibyte,
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

Resolved (details live inline in the sections above; PR #67 review threads
hold the rationale):

- Root stays `package main`; the library splits `database/sql`-style:
  consumer-facing `vm`, implementer contract `backend`, implementations
  under `backend/`.
- Capabilities are standalone package-qualified `-er` interfaces — no
  `Backend` embedding; composed where needed. Core interface named
  `backend.Backend` (accepting the `driver.Driver` stutter).
- No default backend in core, enforced structurally by the import
  direction.
- Guest control comes from `Instance.RemoteControl()`; backend
  constructors select its implementation (`qemu.BackendWithQGA` now,
  `qemu.BackendWithGuest` later), with lazy-vs-eager wiring an
  implementation detail behind the constructor.
- Sizes are typed: public `units` package, `units.Bytes` base with constant
  multipliers (`2048 * units.Mebibyte`); manifest TOML numbers stay
  MiB-denominated, converted by `manifest.Load`.
- Guest file trees move as `CopyToGuest(fs.FS)` / `CopyFromGuest` (streamed
  tar under the hood), with ownership set via `CopyOptions`.

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

- **D9 — tty / interactive sessions** (leaning settled in the PR #67
  thread, pending final call). Current direction:
  - `virtle guest` embeds a **rudimentary sshd** (`x/crypto/ssh`, pure Go —
    the Tailscale-SSH shape): `exec`/`shell`/pty channels only, no sftp or
    forwarding initially. Constrained to vsock, but with publickey auth via
    a host-provisioned key, because vsock guest-local loopback means
    unprivileged guest processes can reach the root daemon's port.
    This gives every daemon-equipped VM the full SSH ecosystem (user's ssh
    config, scp, rsync, VS Code Remote-SSH) with zero image requirements,
    and makes a custom terminal protocol unnecessary — SSH's channel model
    is the terminal protocol. The `./guest` dependency budget amends to
    include `x/crypto/ssh`.
  - SSH remains an *additional service*, not the guest-control transport:
    the virtle RPC stays for programs (typed, versionable, streams), sshd
    serves humans and human-shaped tooling.
  - Core standardizes the session type `vm.Term` (`io.ReadWriteCloser` +
    `Resize` + `Wait`) with `vm.TermOptions`; the serial/chardev console
    surfaces it via a `backend.ConsoleProvider` capability (the no-daemon
    debug path); no transport-driver registry.
  - The CLI keeps today's UX: `virtle launch --ssh` execs the user's ssh
    client, ProxyCommand'd to the daemon's vsock port.

## Appendix: mapping to existing internals

The new API is a re-skin, not a rewrite — each piece has an existing
implementation to be adapted:

| New API piece | Existing code that becomes its implementation |
|---|---|
| `backend/qemu` `Start` | `internal/manager/qemu.go` lowering + `internal/manager/launch` (`BuildPlan`, `AcquireCID`, socket waits) + `internal/executor` supervision |
| `backend.Instance` | `internal/executor.Process` (later, libkrun: a cgo handle) |
| `backend/qemu`'s QGA `vm.Guest` | `internal/qga` client behind the new shapes (`guest-exec` → `Run`, base64 file chunks → `Open`/`Create`) |
| `guest` daemon + client | new code; binary-safe, concurrent, streaming protocol (see protocol requirements) — supersedes both QGA's base64 chunking and `control`'s request/response-only framing |
| `backend.Suspender` | `internal/qmpclient` migration save/restore + `launch.SuspendState` |
| `backend.MemoryResizer` | `internal/balloon` controller / QMP path |
| `backend.DeviceAttacher` | `internal/hotplug` + its QMP adapter |
| `manifest.Load` | `internal/manifest` decode + resolve, retargeted at (Spec, Backend) |

## Non-goals

This document does not freeze the current control-socket wire format or the
suspend-state on-disk format; those are revisited during migration. It also
does not schedule the migration itself — the staging of moving the current
CLI onto this API is follow-up work once the API shape is agreed.
