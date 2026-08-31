# Design: the virtle guest daemon

Status: proposal, sibling to [roadmap.md](roadmap.md) (remaining-work item
2); refs [#66](https://github.com/shazow/virtle/issues/66),
[#67](https://github.com/shazow/virtle/pull/67).

`virtle guest` is a daemon that runs inside the VM and gives the virtle
host typed, streaming remote control plus a real SSH endpoint — replacing
QGA and the QGA-era session scaffolding. The requirements were settled in
#67 review; this document designs the thing itself. The primary prior art
is **Tailscale SSH** (`tailscale/tailscale`, `ssh/tailssh`): an
app-embedded, pure-Go, identity-authenticated sshd for a constrained
network — the same shape as ours with vsock in place of the tailnet.

## Package layout and budget

```
guest/                 Serve (in-guest), Dial + Client (host side)
guest/internal/sshd/   the embedded SSH server: channels, session spawning, sftp
guest/internal/wire/   the virtle subsystem protocol: hello, frames, streams
main.go                `virtle guest [--listen vsock://:PORT|unix://PATH]`
```

- `guest.Serve(ctx, l net.Listener, cfg Config) error` — transport
  injected, so tests drive it over `net.Pipe`; vsock/unix listeners are
  the CLI's job.
- `guest.Dial(ctx, addr) (*Client, error)` — `Client` implements
  `vm.Guest` and `vm.GuestWithCopy` (its first implementation), and grows
  `vm.GuestWithX` extensions later.
- Dependency budget: stdlib + `golang.org/x/sys` (vsock, termios ioctls,
  landlock) + `golang.org/x/crypto/ssh`, plus `gliderlabs/ssh` and
  `pkg/sftp` per D-g2/D-g3. All pure Go; static builds already
  CI-enforced. The daemon ships inside the regular `virtle` binary; a
  minimal `cmd/virtle-guest` main stays available later without build
  tags.

## Architecture: one listener, one mux — SSH is the transport

Everything the daemon serves rides one SSH server on one vsock port,
using SSH's channel multiplexing as *the* mux (the "one mux instead of
two" resolution from #67's transport thread; precedent: sftp and NETCONF
(RFC 6242) are both RPC protocols carried as SSH subsystems):

- **Session channels** (`pty-req`, `shell`, `exec`, `window-change`,
  `env`, `exit-status`/`exit-signal`) serve humans and their tooling —
  the user's ssh client, VS Code Remote-SSH, `virtle launch --ssh` (which
  keeps today's UX: exec the user's ssh, ProxyCommand'd to the port).
- **The `virtle` subsystem** carries the typed RPC for programs
  (`vm.Guest` semantics). Debuggable from a terminal via
  `ssh -s virtle <vm>`.
- **Stream channels** carry bulk data (guest file trees as tar, file
  reads/writes), one channel per operation.
- Explicitly out of scope initially: agent forwarding, port forwarding
  (`direct-tcpip` can be added later for `ssh -L` into guest services),
  session recording.

Concurrency and streaming fall out of the channel model; the remaining
protocol work is framing inside the subsystem: newline-delimited JSON
control frames, with each bulk transfer opening a dedicated stream
channel keyed by request id. `x/crypto/ssh` channel flow-control windows
need deliberate sizing or bulk copies throttle (the classic sftp-vs-http
gap) — treat window tuning as part of the copy implementation, with a
throughput test.

**Version handshake, first.** The subsystem's opening exchange is a hello
(`{"virtle-proto": 1, "version": "v0.x.y"}` both ways) before any
operation: the daemon is baked into guest images and will routinely skew
against the host virtle, so mismatch must fail or degrade explicitly.
Session channels (plain SSH) work regardless of proto version — the debug
path never depends on the handshake.

## Identity and keys

Two independent gates, both cheap:

1. **vsock peer CID.** The daemon checks the connection's peer CID
   (`getsockopt` on the vsock): the host is `VMADDR_CID_HOST` (2); a
   guest-local loopback connection arrives with the guest's own CID.
   Non-host CIDs are rejected before SSH auth begins. This is the virtle
   analog of Tailscale authenticating by tailnet identity (`WhoIs` on the
   connection) instead of passwords.
2. **Publickey auth with a host-provisioned key** as defense-in-depth
   (the daemon runs as root; CID checks alone shouldn't be load-bearing).

Key bootstrap (D-g1): the host generates the client keypair (today's
`sshtools.KeyStore` ed25519 machinery) and delivers the *public* half to
the guest at boot — recommended via **kernel cmdline**
(`virtle.guest.authorized_key=<base64>`, ~100 bytes; virtle controls
direct-kernel boot, and cmdline is readable in-guest which is fine for a
public key), with a file-in-share fallback for non-direct-kernel boots.
No `authorized_keys` writes over a bootstrap protocol, no TOFU.

The daemon's **host key** is generated (ed25519) at first boot and
persisted in the guest; the virtle host pins it on first connection in
its state dir, known_hosts-style. On vsock the hypervisor mediates the
path so MITM isn't in the threat model — pinning is belt-and-suspenders
and makes `ssh` clients quiet.

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
  commands inherited QGA's restricted `PATH`.

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

So the earlier "no sftp initially" scoping note inverts: to make `scp`
(and general file UX) actually work against bare images, **serve an sftp
subsystem**, via the pure-Go `github.com/pkg/sftp` server (the standard
implementation; Tailscale's sshd likewise serves sftp as a subsystem).
One pure-Go dependency added to the budget; sftp sessions run inside the
same credentialed spawn path as exec sessions. Recommended in; the
fallback position is documenting `scp -O`'s guest-binary requirement.

## The virtle subsystem: implementing `vm.Guest`

| Operation | Mechanism |
|---|---|
| `Run` | credentialed exec (SysProcAttr), buffered output + exit status in the response frame |
| `Open` / `Create` | stream channel per file, plain bytes |
| `CopyToGuest` / `CopyFromGuest` | stream channel carrying tar; extraction enforces the zip-slip guard as an invariant (daemon extracts as root) and applies `CopyOptions` overwrite/ownership semantics |
| `Shutdown` | clean daemon stop + system shutdown |
| Ping/readiness | the hello itself |

Every operation runs with an explicit uid/gid (from the request, default
root), through the same spawn path — the RPC gets no ambient
authority the SSH sessions don't.

## Host-side wiring

- `qemu.Guest{Port}` joins the sealed `RemoteControl` union;
  `vmm.Config.GuestAgentDialer` takes the daemon dialer and
  `vmm.Config.GuestReadiness` becomes "Dial + hello succeeded" (replacing
  the ssh-ready socket gate).
- `backend/qemu/session`'s autoprovision path collapses: the key travels
  in the boot cmdline, so there is nothing to install over QGA — the
  first piece of the session-layer demolition (roadmap item 4).

## In-guest lifecycle

The daemon is init-agnostic: images run `virtle guest` from whatever init
they have (a systemd unit and a busybox-init example ship in
getting-started docs). It does not supervise other processes and does not
want to be PID 1; if an image runs it as PID 1 anyway it sets itself as
subreaper and reaps, but that's tolerance, not a feature.

## Testing

Per AGENTS.md: `Serve` over injected listeners (`net.Pipe`) with
`x/crypto/ssh` as the in-process client; the wire protocol tested without
sockets; privilege/spawn operations behind a small interface with a
fake for unit tests, plus root-only integration coverage in the existing
`nix flake check` integration suite; copy semantics against
`fstest.MapFS` archives.

## Decision points

- **D-g1 — key bootstrap**: (a) *recommended:* kernel cmdline param, file
  fallback; (b) file-in-share only; (c) peer-CID-only, no key (rejected:
  daemon runs as root, CID checks shouldn't be load-bearing alone).
- **D-g2 — server layer**: (a) *recommended:* `gliderlabs/ssh` (small,
  pure Go, proven in Gitea/soft-serve; provides session/pty/subsystem
  plumbing); (b) hand-rolled on `x/crypto/ssh` (tightest dependency
  budget, most code to own); (c) `charmbracelet/wish` (rejected: an SSH
  *app* framework with the Charm ecosystem attached — aimed at a
  different problem).
- **D-g3 — sftp subsystem**: (a) *recommended:* include (pkg/sftp;
  makes default `scp` work against bare images); (b) defer and document
  `scp -O`.
- **D-g4 — bulk-stream framing**: (a) *recommended:* one SSH channel per
  operation; (b) interleaved frames inside the subsystem channel (fewer
  channels, hand-rolled flow control — rejected as re-implementing what
  SSH gives us).
- **D-g5 — login semantics**: (a) *recommended:* direct exec with
  `SysProcAttr` credentials only — no `login(1)`/`su` ladder, no PAM,
  no utmp; the semantics minimal sandbox images don't have anyway.
  (b) add a probe-`login`/`su` ladder for full multi-user images — the
  point at which a `--be-child` re-exec becomes worthwhile, since PAM-era
  setup needs a program in the child, not a syscall menu. Deferred until
  an image actually needs it.
