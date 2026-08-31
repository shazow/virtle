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
guest/internal/sshd/   the embedded SSH server: channels, incubator, sftp
guest/internal/wire/   the virtle subsystem protocol: hello, frames, streams
main.go                `virtle guest [--listen vsock://:PORT|unix://PATH]`
```

- `guest.Serve(ctx, l net.Listener, cfg Config) error` — transport
  injected, so tests drive it over `net.Pipe`; vsock/unix listeners are
  the CLI's job.
- `guest.Dial(ctx, addr) (*Client, error)` — `Client` implements
  `vm.Guest` and `vm.GuestWithCopy` (its first implementation), and grows
  `vm.GuestWithX` extensions later.
- Dependency budget: stdlib + `golang.org/x/sys` (vsock, termios ioctls) +
  `golang.org/x/crypto/ssh` (+ `github.com/pkg/sftp` if D-g3 is accepted).
  All pure Go; static builds already CI-enforced. The daemon ships inside
  the regular `virtle` binary; a minimal `cmd/virtle-guest` main stays
  available later without build tags.

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

## The sshd, Tailscale-inspired

What `tailssh` does, and what we take:

- **Layering.** Tailscale builds on its own maintained fork of
  gliderlabs/ssh (`github.com/tailscale/gliderssh`) over `x/crypto/ssh`.
  For virtle (D-g2): build **directly on `x/crypto/ssh`** — our server is
  one subsystem plus session channels with one auth mode; the
  gliderlabs convenience layer isn't worth a fork-maintenance obligation
  at this scope (Tailscale's own fork existing is the cautionary tale).
- **The incubator pattern** — the core steal. `tailscaled` re-execs
  itself (`tailscaled be-child ssh`) per session rather than fiddling the
  daemon's own credentials: the child registers a new OS session, applies
  `setgroups`/`setgid`/`setuid` in that order, **defensively asserts the
  drop is irreversible** (attempts to restore the original uid/gid must
  fail or the process exits), then execs the command. virtle guest does
  the same via a hidden `virtle guest --be-child` re-exec: per-session
  privilege isolation, sensitive parameters passed via fds rather than
  argv/env.
- **The login ladder, inverted for our images.** tailssh tries `login(1)`
  (full PAM, needs a TTY), falls back to `su -w -l -c` (PAM without TTY),
  and only then direct in-process exec after the privilege drop (also its
  SELinux path). virtle guests are typically minimal sandbox images —
  often root-only, no PAM, sometimes no `login`/`su` at all — so
  Tailscale's *last resort is our common case*: probe for `login`/`su`
  and use them when present, otherwise direct exec (D-g5). Same ladder,
  honest default.
- **PTY allocation**: `/dev/ptmx` + termios ioctls via `x/sys` (the
  creack/pty mechanics without the dependency); window-change maps to
  `TIOCSWINSZ`.

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
same incubator privilege drop as exec sessions. Recommended in; the
fallback position is documenting `scp -O`'s guest-binary requirement.

## The virtle subsystem: implementing `vm.Guest`

| Operation | Mechanism |
|---|---|
| `Run` | incubator exec, buffered output + exit status in the response frame |
| `Open` / `Create` | stream channel per file, plain bytes |
| `CopyToGuest` / `CopyFromGuest` | stream channel carrying tar; extraction enforces the zip-slip guard as an invariant (daemon extracts as root) and applies `CopyOptions` overwrite/ownership semantics |
| `Shutdown` | clean daemon stop + system shutdown |
| Ping/readiness | the hello itself |

Every operation runs with an explicit uid/gid (from the request, default
root), through the same incubator path — the RPC gets no ambient
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
sockets; incubator privilege operations behind a small interface with a
fake for unit tests, plus root-only integration coverage in the existing
`nix flake check` integration suite; copy semantics against
`fstest.MapFS` archives.

## Decision points

- **D-g1 — key bootstrap**: (a) *recommended:* kernel cmdline param, file
  fallback; (b) file-in-share only; (c) peer-CID-only, no key (rejected:
  daemon runs as root, CID checks shouldn't be load-bearing alone).
- **D-g2 — server layer**: (a) *recommended:* hand-rolled on
  `x/crypto/ssh`; (b) gliderlabs/ssh (or a fork) — convenience now,
  maintenance obligation later.
- **D-g3 — sftp subsystem**: (a) *recommended:* include (pkg/sftp;
  makes default `scp` work against bare images); (b) defer and document
  `scp -O`.
- **D-g4 — bulk-stream framing**: (a) *recommended:* one SSH channel per
  operation; (b) interleaved frames inside the subsystem channel (fewer
  channels, hand-rolled flow control — rejected as re-implementing what
  SSH gives us).
- **D-g5 — session privilege ladder**: (a) *recommended:* probe
  `login`/`su`, fall back to direct exec after drop (auto); (b) always
  direct exec (simplest, loses PAM on full-featured images).
