# History

Day-by-day highlights of what has changed in virtle since the [v0.1
release](https://github.com/shazow/virtle/releases/tag/v0.1) (2026-07-13),
newest first. The focus is on consumer-facing changes — new features, changed
behavior, fixed bugs — with substantial internal changes noted briefly.

## 2026-08-08

- **`virtle` with no command now prints the full help text** instead of a bare
  "Please specify one command" error, `--help` output goes to stdout, and a new
  help trailer points at per-command `--help` and the project repo. (#64)
- **Graceful guest shutdown**: stopping a VM now asks the guest to power off via
  the guest agent (`guest-shutdown`, or a custom `shutdown_exec` manifest
  override) before falling back to QMP quit. A new `shutdown_timeout` manifest
  key (default 90s) bounds the wait, and a quick agent ping probe skips the wait
  entirely for agent-less or paused guests. (#61)
- More fixes in the same batch (#61):
  - File-backed VM saves are no longer wrongly capped by the 5-second RPC
    liveness bound — long migrations run to completion again.
  - `virtle manifest defaults` output now round-trips durations through TOML
    correctly (500ms previously re-decoded as ~15.8 years).
  - Hotplug attach rollback no longer leaks or double-frees backends when a
    request is cancelled mid-attach.
  - Abandoned control-socket `guest.exec` calls are cancelled when the peer
    disconnects instead of polling forever.
  - `ssh.retry_delay` must now be greater than zero; `manifest.schema.json`
    regenerated with self-documenting defaults and required fields.
  - Internally: new shared `qmpwire.Session` socket-session machinery for both
    the QMP and guest-agent clients, context-driven process stop escalation,
    and a jsonschema library swap.
- **`nix build .#virtle` fixed**: a stale `vendorHash` left the flake build
  failing with "go: inconsistent vendoring"; CI now runs `nix build` on every
  PR/push so flake regressions are caught at review time. (#62, contributed by
  0xferrous)
- Installing files into a guest is faster: guest directory trees are now
  created with a single scripted guest-exec instead of per-directory
  `test`/`install` round-trips, contained behind a new installer seam with
  integration tests. (#65, core change contributed by 0xferrous)

## 2026-08-07

- **Guest-agent timeouts reworked around context deadlines.** A wedged guest
  agent now fails fast (per-RPC liveness bound) even for commands with no time
  limit, and timeout errors distinguish "command too slow" from "agent
  unresponsive" from "caller cancelled". New manifest key
  `qemu.guest_default_timeout` (seconds, default 30, 0 disables), and the
  control-socket `guest-exec` RPC accepts an optional `timeout` duration string
  like `"30s"`. Large internal rework: +731/-306 across 39 files. (#59)
- **Breaking manifest rename** in the follow-up batch: balloon keys
  `poll_interval_seconds` / `reclaim_holdoff_seconds` became `poll_interval` /
  `reclaim_holdoff` (float seconds). Also fixed: an explicit
  `ssh.retry_delay = 0` now means "retry immediately" instead of being silently
  replaced by the default. The QMP client got the same context-deadline
  treatment, bounding migration save/restore by the configured migration
  timeout. (#60)
- Startup logs now include the full shell-quoted QEMU command line, so a failed
  launch can be reproduced by copy-pasting the command. (#58, contributed by
  0xferrous)

## 2026-08-01

- **`virtle launch` no longer rewrites the manifest file.** Previously a
  relative `working_dir` was absolutized and persisted back into the manifest,
  destroying comments/formatting, materializing omitted fields, resetting file
  permissions to 0644, and freezing the first launch's CWD forever. The
  manifest is now opened read-only and resolved in memory: `working_dir = "."`
  (and the empty default) means the current directory on every run, and
  `launch`, `suspend`, and `rpc` all share one loader so they agree on path
  resolution. (#51)
- Wedged helper processes (e.g. a virtiofsd stuck in uninterruptible sleep) can
  no longer hang VM teardown forever — the post-SIGKILL wait is bounded, with a
  background reaper collecting stragglers. (#51)
- Hotplug attach now resolves only the requested device, so an unrelated
  malformed manifest entry no longer blocks attaching a valid one. Internally
  the hotplug package was restructured around explicit per-kind attach/release
  plans. (#51)
- Bug-fix sweep (#49):
  - The readiness token size cap is actually enforced — oversized guest output
    now errors instead of buffering indefinitely.
  - The auto-balloon controller survives transient QMP errors instead of
    stopping.
  - `free_page_reporting = false` in the manifest is honored (was silently
    ignored).
  - SSH auth-failure detection no longer over-matches other SSH errors, and the
    SSH key directory is created with 0700 permissions.
  - Internally: dead-code removal, logger injection replacing global setters,
    and manifest device-model unification (mount validation errors now use the
    `manifest.qemu.devices.mounts[i]` path form).

## 2026-07-26

- SIGTERM during an active launch now triggers the full teardown path — helpers
  stopped, QEMU asked to quit over QMP, runtime state cleaned up — instead of
  leaving orphaned processes. `kill` and service-manager termination of virtle
  shut VMs down cleanly. (#43, contributed by 0xferrous)

## 2026-07-24

- **v0.2 released**, shipping everything below since v0.1.
- **Interactive serial console modes.** The manifest's
  `serial = "off" | "print" | "console"` setting now maps to distinct console
  behaviors: `print` streams boot/kernel output without grabbing host stdin,
  while `console` connects stdin for interactive use through a muxed QEMU
  chardev — Ctrl-A x force-exits QEMU, Ctrl-A Ctrl-A sends a literal Ctrl-A,
  and Ctrl-C no longer kills the VM in console mode. (#38, fixing the
  interactive-stdin problem reported in #31)
- New getting-started examples: a "Tiny NixOS" fast-boot experiment (#37) and a
  complete Docker-image-to-VM walkthrough showing a container root filesystem
  booting as a VM disk (#39).

## 2026-07-23

- **virtle works with non-NixOS guests.** Guest-agent commands (`mount`,
  `chmod`, `install`, …) previously invoked hardcoded
  `/run/current-system/sw/bin/` paths; they now use bare command names resolved
  by the guest's own PATH, unblocking Alpine and Docker-image guests. The
  manifest also defaults `ssh.exec` to `["ssh"]` so SSH launch works without
  declaring an exec. (#33)
- The serial console became actually interactive: with a stdio chardev, QEMU
  now inherits stdin and stays in the foreground process group, so typing into
  the console works. (#35)
- **Getting Started guide added**: `docs/getting-started/` with a full NixOS
  example (bootable guest flake, guest-agent + automatic SSH key provisioning)
  and an Alpine example booting a non-NixOS guest to a serial console. Later
  revised the same day into a two-step flow — boot to a minimal console first,
  then layer on SSH — with the NixOS example switched to public-key-only auth
  so automatic key provisioning triggers deterministically. (#34, #35, #36, and
  direct commits)
- `ssh.retry_delay` is honored for delay-only lifecycle waits — SSH connection
  retries are paced by the configured delay instead of hammering the guest.
  (direct commit)
- `--ssh` now fails fast with a clear error when `manifest.ssh.exec` is empty.
  (#34)

## 2026-07-20 – 2026-07-22

- The virtle turtle mascot arrived in the README (and was promptly upgraded to
  a better turtle). (direct commits)

## 2026-07-13 – 2026-07-14

- README fix: the virtiofs template-variable path corrected to
  `mounts[type=virtiofs].virtiofs`. (direct commit)
- Release pipeline hardened after v0.1: tag-format validation runs first, the
  checked-out commit is verified against the tag, publication is idempotent
  (re-runs upload assets with `--clobber` instead of failing), and the workflow
  supports manual retries. Internal, but it's why release artifacts publish
  reliably. (#28 and direct commits)
