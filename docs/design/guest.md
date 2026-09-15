# Design: the virtle guest daemon

Status: proposal, sibling to [roadmap.md](roadmap.md) (remaining-work item
1); refs [#66](https://github.com/shazow/virtle/issues/66),
[#67](https://github.com/shazow/virtle/pull/67). Revised for two backends
after [#94](https://github.com/shazow/virtle/pull/94) added Firecracker.

`virtle guest` is a daemon that runs inside the VM and gives the virtle
host typed, streaming remote control plus a real SSH endpoint — replacing
QGA on QEMU and providing the first guest transport of any kind on
Firecracker. The requirements were settled in #67 review; this document
designs the thing itself. The primary prior art is **Tailscale SSH**
(`tailscale/tailscale`, `ssh/tailssh`): an app-embedded, pure-Go,
identity-authenticated sshd for a constrained network — the same shape as
ours with vsock in place of the tailnet.

## Package layout and budget

```
guest/                 Server (in-guest), Dialer + Client (host side)
guest/internal/sshd/   the embedded SSH server: channels, session spawning, sftp
guest/internal/wire/   the virtle subsystem protocol: hello, frames, streams
main.go                `virtle guest [--listen vsock:PORT|unix:PATH] [init]`
```

- `guest.Server` — the `http.Server` / `tsnet.Server` shape: an exported
  struct (`HostKey`, `AuthorizedKeys`, `Logger`), zero value usable with a
  lazily generated host key, `Serve(l net.Listener)` and `Shutdown(ctx)`.
  Listeners are the caller's job; tests drive it over `net.Pipe`.
- `guest.Dialer{DialContext func(ctx) (net.Conn, error)}` — the host side
  injects the transport, so `guest` knows nothing about vsock, Unix
  sockets, or either VMM. `Dialer.Dial(ctx) (*Client, error)`; `Client`
  implements `vm.Guest` and `vm.GuestWithCopy`, and grows `vm.GuestWithX`
  extensions later.
- Dependency budget: stdlib + `golang.org/x/sys` (vsock, termios ioctls,
  landlock) + `golang.org/x/crypto/ssh`, plus `gliderlabs/ssh` and
  `pkg/sftp` per D-g2/D-g3. All pure Go; static builds already
  CI-enforced. The daemon ships inside the regular `virtle` binary; a
  minimal `cmd/virtle-guest` main stays available later without build
  tags. One binary per guest architecture: Firecracker requires guest arch
  = host arch, so the host's own `virtle` build is always the right one.

## Architecture: one listener, one mux — SSH is the transport

Everything the daemon serves rides one SSH server on one vsock port,
using SSH's channel multiplexing as *the* mux (precedent: sftp and NETCONF
(RFC 6242) are both RPC protocols carried as SSH subsystems):

- **Session channels** (`pty-req`, `shell`, `exec`, `window-change`,
  `env`, `exit-status`/`exit-signal`) serve humans and their tooling —
  the user's ssh client, VS Code Remote-SSH, `virtle launch --ssh`.
- **The `virtle` subsystem** carries the typed RPC for programs
  (`vm.Guest` semantics). Debuggable from a terminal via
  `ssh -s virtle <vm>`.
- **Stream channels** carry bulk data (guest file trees as tar, file
  reads/writes), one channel per operation.
- Explicitly out of scope initially: agent forwarding, port forwarding
  (`direct-tcpip` can be added later for `ssh -L` into guest services),
  session recording.

Framing inside the subsystem: newline-delimited JSON control frames, with
each bulk transfer opening a dedicated stream channel keyed by request
id. `x/crypto/ssh` channel flow-control windows need deliberate sizing or
bulk copies throttle — treat window tuning as part of the copy
implementation, with a throughput test.

**Version handshake, first.** The subsystem's opening exchange is a hello
(`{"virtle-proto": 1, "version": "v0.x.y"}` both ways) before any
operation: the daemon is baked into guest images and will routinely skew
against the host virtle, so mismatch must fail or degrade explicitly.
Session channels (plain SSH) work regardless of proto version — the debug
path never depends on the handshake.

**The hello is readiness.** Today QEMU gates the session on an `SSH-READY`
token the image writes to a readiness socket, and Firecracker guests are
observed by scanning the console for a marker line. For daemon guests both
collapse into one backend-neutral rule: the machine is ready when the
daemon's hello succeeds. `internal/session`'s `Ready` hook gets that as
its default for any machine whose `RemoteControl` is the daemon, and the
per-backend readiness hooks go (roadmap item 5).

## Transport: vsock on both VMMs, dialed differently

The guest side is the same everywhere: the daemon listens on
`AF_VSOCK` at a fixed port and needs `CONFIG_VIRTIO_VSOCKETS=y` (which
pulls `CONFIG_NET` and `CONFIG_VSOCKETS`) and `/dev/vsock`. The host side
differs per VMM, which is exactly why `guest.Dialer` takes a dial
function rather than an address string:

| VMM | Host-side mechanism | Dial function |
|---|---|---|
| QEMU (`vhost-vsock`) | The host kernel speaks `AF_VSOCK`: connect to `(guest CID, port)`. | `x/sys/unix` vsock socket; lives in `backend/qemu` |
| Firecracker | No host `AF_VSOCK`. The device is configured through the API (`PUT /vsock` with `guest_cid` and `uds_path`); a host process connects to the Unix socket at `uds_path`, writes `CONNECT <port>\n`, and reads `OK <assigned_port>\n` before the byte stream begins. Guest-initiated connections land on `<uds_path>_<port>`. | Unix dial plus the two-line handshake; lives in `backend/firecracker` |

Each backend contributes its dial function when it wires
`Machine.RemoteControl`; `guest` never learns which VMM it is talking to,
and tests use `net.Pipe`.

### Kernel requirements for minimal guests

The tinyconfig kernel in `tests/e2e/fixtures/fast` has serial, virtio
MMIO/block/console, and ext4 — and *no networking at all* (`CONFIG_NET`
off), so it cannot host the daemon as-is. The "userspace" fixture profile
adds `NET` and `UNIX`. The daemon needs, on top of that,
`CONFIG_VSOCKETS` and `CONFIG_VIRTIO_VSOCKETS`; the fixture's
`kernel.nix` should grow a `guest` profile carrying them so the daemon is
exercised by the same e2e checks. Distribution kernels (Alpine, NixOS,
Ubuntu) already have all of it.

A serial or virtio-serial transport was considered for kernels without
`NET` and rejected as the primary path: Firecracker exposes a single
serial port already used for the console, and QEMU's virtio-serial
channel would make the daemon QEMU-shaped again. vsock is the one
transport both VMMs share; the kernel cost is small.

### SSH clients need a ProxyCommand on Firecracker

`ssh user@vsock/<cid>` works on QEMU hosts only because a host-side
`AF_VSOCK` exists (via systemd's ssh proxy or equivalent). On Firecracker
nothing on the host speaks vsock, so the CLI provides the bridge itself:
`virtle launch --ssh` runs the user's ssh client with `ProxyCommand=virtle
guest proxy --machine <state dir>`, a hidden subcommand that dials the
daemon's sshd through the machine's backend-specific dial function and
pipes stdio. The same ProxyCommand works on QEMU, which removes the
`vsock/<cid>` destination syntax and the `sshtools.VSockDestination`
special case from the CLI, and gives every other SSH-speaking tool (scp,
rsync, VS Code) one configuration line per machine.

## Identity and keys

Two independent gates, both cheap:

1. **vsock peer CID.** The daemon checks the connection's peer CID: the
   host is `VMADDR_CID_HOST` (2) under both VMMs (Firecracker's device
   presents host connections with CID 2 as well); a guest-local loopback
   connection arrives with the guest's own CID. Non-host CIDs are rejected
   before SSH auth begins. This is the virtle analog of Tailscale
   authenticating by tailnet identity (`WhoIs` on the connection) instead
   of passwords.
2. **Publickey auth with a host-provisioned key** as defense-in-depth
   (the daemon runs as root; CID checks alone shouldn't be load-bearing).

Key bootstrap (D-g1): the host generates the client keypair (today's
`sshtools.KeyStore` ed25519 machinery) and delivers the *public* half to
the guest at boot via the **kernel command line**
(`virtle.guest.authorized_key=<base64>`, ~100 bytes; virtle already
assembles the command line for both VMMs and it is readable in-guest,
which is fine for a public key), with a file-in-share fallback on QEMU
for images that boot through their own bootloader. No `authorized_keys`
writes over a bootstrap protocol, no TOFU.

The daemon's **host key** is generated (ed25519) at first boot and
persisted in the guest where the image allows; on an immutable or
initramfs root it is regenerated per boot and the host does not pin it
(the hypervisor mediates the path, so MITM is not in the threat model;
pinning is belt-and-suspenders that only a persistent guest can offer).

## The sshd

**Server layer (D-g2)**: build on **`gliderlabs/ssh`** — the small,
widely-proven convenience layer over `x/crypto/ssh` (Gitea and soft-serve
run on it) that provides exactly the plumbing we'd otherwise hand-roll:
session channels, pty request handling, the subsystem handler map, one
extra pure-Go dependency. `charmbracelet/wish` sits a level above it as a
middleware/app framework for building SSH *apps* (with the Charm
ecosystem attached) — more framework than a guest daemon wants. Building
directly on `x/crypto/ssh` remains the fallback if the dependency budget
is contested; the plumbing is bounded.

**Process model — no re-exec needed for the core.** Sessions run as a
requested user via plain `exec.Cmd` with
`SysProcAttr{Credential, Setsid, Setctty}`: Go's fork/exec path applies
`setgroups`/`setgid`/`setuid`, creates the new session, and assigns the
controlling TTY *in the child, between fork and exec* — per-child
credentials without the daemon ever touching its own, and without
re-exec. (Tailscale's incubator re-exec earns its keep for what lies
beyond that fixed syscall menu — PAM via `login(1)`/`su`, utmp
registration, SELinux contexts on full multi-user systems. Those are
exactly the semantics minimal sandbox images don't have, so we defer
them; see D-g5 for the path back.)

- **PTY allocation**: `/dev/ptmx` + termios ioctls via `x/sys` (the
  creack/pty mechanics without the dependency); window-change maps to
  `TIOCSWINSZ`; the slave becomes the child's controlling TTY via
  `Setctty`.
- **Session teardown**: `Setsid` gives every session its own process
  group; closing the session channel kills the group and a pidfd wait
  reaps it, so background pipelines don't outlive their session.
- **Environment**: `HOME`/`SHELL`/`USER` resolved via pure-Go `os/user`
  (static builds read `/etc/passwd` directly), `PATH` from a sane
  baseline — the lesson already paid for in v0.3.x, where internal guest
  commands inherited QGA's restricted `PATH`. BusyBox-only guests have no
  `/etc/passwd`: fall back to root with `HOME=/` and `SHELL=/bin/sh`.

### Landlock, and what it does not replace

Landlock is an *additive restriction* LSM (filesystem, plus TCP
bind/connect on newer kernels; 5.13+; syscalls available in
`x/sys/unix`, within budget). It cannot switch identity: a
landlocked-but-root session is still uid 0 for file ownership, signals,
and everything else. So Landlock is not an alternative to the incubator —
but that's because *credentials* were the requirement, and
`SysProcAttr.Credential` already covers them without re-exec. The two
mechanisms are orthogonal: credentials say *who* the session is, Landlock
says *what it may touch*.

Where Landlock does fit: opt-in **confined sessions and operations**
(e.g., restricting a session or a `CopyFromGuest` to a subtree) as a
future extension. One wrinkle to record now: Landlock self-restriction is
one-way and applies to the calling thread/process, and `SysProcAttr` has
no hook for it — applying it per-child without poisoning the daemon (or a
pooled runtime thread) requires a tiny `virtle guest --be-child` shim
that restricts itself and then execs the payload. That is the one place a
re-exec comes back: for restriction, not identity. Relatedly,
`no_new_privs` is tempting hardening for non-root sessions but breaks
`sudo` inside them — off by default.

### scp, rsync, and the sftp question (D-g3)

A reality check against "the SSH ecosystem works with zero image
requirements": modern OpenSSH `scp` **defaults to the SFTP protocol** —
without an sftp subsystem, plain `scp` fails unless the user passes `-O`,
and `-O` runs an `scp` binary guest-side (an image requirement). `rsync`
always requires rsync in the guest; nothing server-side fixes that.

So to make `scp` (and general file UX) actually work against bare images,
**serve an sftp subsystem**, via the pure-Go `github.com/pkg/sftp` server
(the standard implementation; Tailscale's sshd likewise serves sftp as a
subsystem). One pure-Go dependency added to the budget; sftp sessions run
through the same credentialed spawn path as exec sessions. Recommended in;
the fallback position is documenting `scp -O`'s guest-binary requirement.

## The virtle subsystem: implementing `vm.Guest`

| Operation | Mechanism |
|---|---|
| `Run` | credentialed exec (`SysProcAttr`); stdout/stderr streamed to the caller's writers as they arrive, exit status in the closing frame (`*vm.ExitError` on the host) |
| `Open` / `Create` | stream channel per file, plain bytes |
| `CopyToGuest` / `CopyFromGuest` | stream channel carrying tar; extraction under `os.Root` (Go 1.24+, with `Chown`/`Symlink`/`MkdirAll` on 1.25), which makes the zip-slip guard traversal-safe by construction, and applies `CopyOptions` overwrite/ownership semantics |
| `Shutdown` | clean daemon stop, then the guest reboots or powers off as the VMM needs — `reboot` on Firecracker (with `reboot=k` the reset exits the VMM; no i8042, no `SendCtrlAltDel`, and it works on aarch64), `poweroff` on QEMU |
| Ping/readiness | the hello itself |

Every operation runs with an explicit uid/gid (from the request, default
root), through the same spawn path — the RPC gets no ambient authority
the SSH sessions don't.

## Host-side wiring

- `backend/qemu`: `qemu.Guest{Port}` joins the sealed `RemoteControl`
  union; its dial function opens `AF_VSOCK` to the machine's CID through
  the existing `vmm.Config.GuestAgentDialer`-style seam (retargeted from
  QGA to the daemon client). `DisableVSock` and `qemu.Guest{}` are
  mutually exclusive, validated at `Start`.
- `backend/firecracker`: `Backend.Guest *Guest` (or a `Guest{Port}` field
  in the same sealed-union style once Firecracker has two transports)
  configures the vsock device through the API before `InstanceStart`,
  keeps `uds_path` in the private `virtle-fc-*` directory, and wires the
  `CONNECT` handshake into the dial function. `RemoteControl` stops
  returning `ErrUnsupported`, and `Spec.Files` works on Firecracker.
- Both: the daemon hello becomes the neutral `Ready` default in
  `internal/session`; `Shutdown` asks the daemon first and falls back to
  the VMM mechanism when there is no daemon. `virtle guest proxy` is the
  ProxyCommand described above.
- `backend/backendtest`: the conformance suite gains `RemoteControl`
  sub-tests that run wherever a daemon is configured — against the
  in-memory backend with a `vmtest.Guest`, and under the integration tag
  against both VMMs once the e2e fixture carries the daemon (below).

## Getting the daemon into minimal guests

Everything above assumes `virtle guest` is running in the guest. The
guests we boot are mostly *not* distributions: `tests/e2e/fixtures/fast`
is a static BusyBox tree packed as an initramfs and as a 16 MiB ext4 root
disk; the Firecracker recipe is a BusyBox initrd plus a raw data disk; the
Alpine, tiny-NixOS, and Docker-image recipes replace the image's init
with a BusyBox inittab. None runs a package manager at boot, most have no
network, some have a read-only root. The strategies, cheapest first,
with the layered recommendation at the end:

**S1 — Build-time fusion (Nix and image recipes).** The fixture and recipes
already assemble the guest tree in a derivation; adding `bin/virtle` (the
static host build) and one inittab line (`::respawn:/bin/virtle guest`)
is a few lines, and a `virtle.lib.guestTree` helper in the flake can do it
for any recipe. Cost: initrd size (+~9 MiB for the full binary, +~3 MiB
with `cmd/virtle-guest`) and its decompression at boot, measurable on
the 128 MiB fixture and worth benchmarking. This is the right answer for
images we build ourselves, and the fixture should take it first so the
daemon is exercised by the e2e checks.

**S2 — Launch-time initramfs overlay (no image rebuild).** The Linux
kernel unpacks concatenated cpio archives in order, so virtle can append
a tiny archive containing `/virtle` and a hook to the user's initrd at
`Start` — a temporary file both VMMs accept as the initrd path, generated
per boot (or cached per virtle version + initrd hash). The image builder
does nothing. The hook is the question: the daemon has to be *started* by
something, which leads to S3.

**S3 — `virtle guest init`: an early-userspace shim.** For guests virtle
boots directly (kernel + initrd or kernel + root disk, which is both of
our VMMs' primary mode), virtle supplies the earliest userspace itself:
the overlay from S2 carries `/init` → `virtle guest init`, which mounts
`devtmpfs`/`proc`/`sysfs`, starts the daemon, and then execs the image's
own init (the initramfs's original `/init`, or `switch_root` onto the
`root=` device and `/sbin/init`). Kata Containers' `kata-agent` and
Bottlerocket run their agents exactly this way — as the first process of
a purpose-built guest. This inverts the earlier stance that the daemon
should never be PID 1: as a *shim that hands off*, being first is the
feature, and it needs only what `/init` scripts already need (a kernel
with initramfs support, no shell). The daemon itself keeps running as a
child of the real init, which reaps it; only the shim is PID 1, briefly.
Images with their own bootloader (the Ubuntu cloud image path in
`docs/recipes`) are outside this mechanism and fall back to S1 or S4.

**S4 — Disk injection plus console bootstrap (no image cooperation at
all).** Attach a small read-only raw image carrying `virtle` as an extra
`vm.Disk` (built once per virtle version, cached), then drive the guest's
BusyBox shell on the serial console — which the e2e check already does on
both backends through `Machine.Console` — to `mount /dev/vdb /mnt &&
/mnt/virtle guest &`. Works on both VMMs with nothing in the image beyond
a shell on ttyS0 and `virtio_blk` + `ext4` (both in the tinyconfig
kernel). Fragile by nature (prompt detection, root shell required); it is
the fallback for foreign images, not a path we design around.

Rejected: shipping the daemon over the serial console (115200 baud makes
a 3 MiB transfer take minutes), and baking it into the kernel's built-in
initramfs (`CONFIG_INITRAMFS_SOURCE`, which couples every kernel build to
a virtle version); a QEMU-only share is not portable to Firecracker.

**Recommendation (D-g6).** Layer them: S1 for the fixture and recipes now,
so the daemon lands with tests; S2+S3 as the general mechanism, since it
makes any directly-booted kernel+initrd or kernel+rootfs guest a daemon
guest with zero image changes, on both VMMs, and it is also where the
kernel-command-line key delivery naturally lives; S4 documented as the
escape hatch. An `Options`-level switch (`Guest: guest.Inject`) on both
backends selects S2+S3; images that already carry the daemon (S1) set
`Guest: guest.Provided`.

## In-guest lifecycle

Init-agnostic by design: the daemon runs from whatever init the image has
(a systemd unit, a BusyBox inittab line, or the S3 shim's exec chain), does
not supervise other processes, and does not want to be a long-lived PID 1.
If an image runs it as PID 1 anyway it sets itself as subreaper and reaps,
but that is tolerance, not a feature; S3's shim is the designed way to be
first.

## Testing

Per AGENTS.md: `Server.Serve` over injected listeners (`net.Pipe`) with
`x/crypto/ssh` as the in-process client; the wire protocol tested without
sockets; spawn/privilege operations behind a small interface with a fake
for unit tests, plus root-only integration coverage in the existing
`integration`-tagged checks; copy semantics against `fstest.MapFS`
archives extracted under `os.Root`; `testing/synctest` for handshake and
timeout behavior. End to end: the e2e fixture's `guest` kernel profile
plus S1 packing boots the daemon on both VMMs under KVM, and the
`backendtest` `RemoteControl` sub-tests run against it.

## Decision points

- **D-g1 — key bootstrap**: (a) *recommended:* kernel command line, file
  fallback; (b) file-in-share only (QEMU-only); (c) peer-CID-only, no key
  (rejected: daemon runs as root, CID checks shouldn't be load-bearing
  alone).
- **D-g2 — server layer**: (a) *recommended:* `gliderlabs/ssh`; (b)
  hand-rolled on `x/crypto/ssh` (tightest dependency budget, most code to
  own); (c) `charmbracelet/wish` (rejected: an SSH *app* framework aimed
  at a different problem).
- **D-g3 — sftp subsystem**: (a) *recommended:* include (pkg/sftp; makes
  default `scp` work against bare images); (b) defer and document
  `scp -O`.
- **D-g4 — bulk-stream framing**: (a) *recommended:* one SSH channel per
  operation; (b) interleaved frames inside the subsystem channel
  (rejected as re-implementing what SSH gives us).
- **D-g5 — login semantics**: (a) *recommended:* direct exec with
  `SysProcAttr` credentials only; (b) a probe-`login`/`su` ladder for full
  multi-user images — the point at which a `--be-child` re-exec becomes
  worthwhile. Deferred until an image needs it.
- **D-g6 — daemon injection**: (a) *recommended:* S1 for owned images,
  S2+S3 (initramfs overlay + `virtle guest init` shim) as the general
  mechanism, S4 as the escape hatch; (b) S1 only, documenting that every
  image must carry the daemon (simplest, pushes the work onto every image
  builder, and leaves foreign images without guest control).
- **D-g7 — transport on `NET`-less kernels**: (a) *recommended:* require
  vsock (`NET` + `VSOCKETS` + `VIRTIO_VSOCKETS`) and add it to the
  fixture's kernel profile; (b) a serial transport for the smallest
  kernels (rejected as primary: single serial port on Firecracker, and it
  would re-couple the daemon to QEMU's virtio-serial).
