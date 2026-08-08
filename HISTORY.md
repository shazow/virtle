# History

Brief changes grouped by day, newest first. Consumer-facing changes lead;
substantial internal changes get a line. Include diffs or before/after examples
when illustrative of the change.

## 2026-08-08

- Bare `virtle` prints full help instead of an error; `--help` goes to stdout. (#64)

  ```console
  $ virtle
  Usage:
    virtle [OPTIONS] <command>
  ...
  Run 'virtle <command> --help' for more information on a command.
  ```

- Graceful guest shutdown: `guest-shutdown` via the agent before QMP quit,
  skipped for agent-less guests via a 1s ping probe. (#61)

  ```toml
  [qemu]
  shutdown_timeout = "90s"                      # new; "0" skips the wait
  shutdown_exec = ["/bin/sh", "-c", "poweroff"] # new, optional override
  ```

- Fixes in the same batch (#61): file-backed VM saves no longer capped at 5s;
  `virtle manifest defaults` durations round-trip through TOML (500ms
  previously re-decoded as ~15.8 years); hotplug rollback no longer leaks
  backends on cancel; abandoned `guest.exec` RPC calls are cancelled on peer
  disconnect; `ssh.retry_delay` must be > 0. Internal: shared `qmpwire.Session`
  for QMP + guest-agent clients, jsonschema library swap.
- `nix build .#virtle` fixed (stale `vendorHash`); CI now runs `nix build` on
  every PR. (#62, 0xferrous)
- Guest file installs are faster: directory trees created in one scripted
  guest-exec instead of per-directory round-trips. (#65, core by 0xferrous)

## 2026-08-07

- Guest-agent timeouts reworked around context deadlines; a wedged agent fails
  fast, and errors distinguish slow command vs. dead agent vs. cancelled
  caller. (#59)

  ```toml
  [qemu]
  guest_default_timeout = "30s"  # new; "0" disables
  ```

  The control-socket `guest-exec` RPC also accepts `"timeout": "30s"`.

- **Breaking**: balloon manifest keys renamed. (#60)

  ```diff
   [balloon.controller]
  -poll_interval_seconds = 5
  -reclaim_holdoff_seconds = 30
  +poll_interval = "5s"
  +reclaim_holdoff = "30s"
  ```

- `ssh.retry_delay = 0` now means "retry immediately" instead of being
  silently replaced by the default. (#60)
- Startup logs include the full shell-quoted QEMU command line — copy-paste to
  reproduce a failed launch. (#58, 0xferrous)

## 2026-08-01

- `virtle launch` no longer rewrites the manifest file. It used to persist an
  absolutized `working_dir` back to disk — destroying comments and formatting,
  materializing omitted fields, resetting permissions to 0644, and freezing the
  first launch's CWD forever. The manifest is now opened read-only;
  `working_dir = "."` means the current directory on every run, and `launch`,
  `suspend`, and `rpc` share one loader. (#51)
- Wedged helpers (e.g. stuck virtiofsd) can no longer hang teardown forever —
  the post-SIGKILL wait is bounded. (#51)
- Hotplug attach resolves only the requested device; an unrelated malformed
  manifest entry no longer blocks it. (#51)
- Fix sweep (#49): readiness token size cap enforced; auto-balloon controller
  survives transient QMP errors; `free_page_reporting = false` honored (was
  ignored); SSH auth-failure detection no longer over-matches; SSH key dir
  created 0700. Internal: logger injection, manifest device-model unification.

## 2026-07-26

- SIGTERM during an active launch runs full teardown (stop helpers, QMP quit,
  clean state) instead of orphaning processes. (#43, 0xferrous)

## 2026-07-24

- **v0.2 released.**
- Serial console modes untangled — `serial` now maps to distinct behaviors. (#38)

  ```toml
  [kernel]
  serial = "off"      # no console
  serial = "print"    # stream boot output, stdin untouched
  serial = "console"  # interactive: Ctrl-A x exits QEMU, Ctrl-A Ctrl-A sends Ctrl-A
  ```

  Ctrl-C no longer kills the VM in console mode.

- New examples: Tiny NixOS fast-boot (#37) and Docker-image-to-VM (#39).

## 2026-07-23

- Non-NixOS guests work: guest-agent commands no longer hardcode NixOS paths. (#33)

  ```diff
  -/run/current-system/sw/bin/mount ...
  +mount ...   # resolved by the guest's own PATH
  ```

  `ssh.exec` also defaults to `["ssh"]`, so SSH launch works undeclared.

- Serial console became actually interactive — QEMU inherits stdin with a
  stdio chardev, so typing works. (#35)
- Getting Started guide added: `docs/getting-started/` with NixOS (guest agent
  + auto SSH key provisioning) and Alpine (console-only) examples, revised the
  same day into a boot-to-console-first flow. (#34, #35, #36)
- `ssh.retry_delay` honored for delay-only waits — retries paced instead of
  hammering the guest.
- `--ssh` fails fast when `manifest.ssh.exec` is empty. (#34)

## 2026-07-20 – 2026-07-22

- The virtle turtle mascot arrived in the README (then got upgraded).

## 2026-07-13 – 2026-07-14

- **v0.1 released** — the first tagged release.
- Tests and CI added: GitHub Actions test workflow, a release workflow, and
  example-manifest validation tests. (#27)
- README fix: virtiofs template path is `mounts[type=virtiofs].virtiofs`.
- Release pipeline hardened: tag verified against checkout, publication
  idempotent (`gh release upload --clobber`), manual retries supported. (#28)

## 2026-07-01

- virtle became its own project: the codebase (~32k lines across 179 files)
  was migrated out of [agentspace](https://github.com/shazow/agentspace)'s
  `virtie` tree into this repo, and lingering `virtie` references renamed. (#1, #2)
