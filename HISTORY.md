# History

Curated newest-first history of breaking changes and important consumer-facing
capabilities, grouped by day. Describe changes from the consumer's perspective,
leading with the affected command or API rather than implementation details.
Keep entries terse. When a day includes both CLI and library changes, group
them by type, CLI first. For compatibility-breaking usage migrations, include
compact before/after examples.

## 2026-09-03

- `virtle launch --ssh` now exits 1, not 255, when the SSH client is killed by
  a signal; ordinary SSH exit statuses still pass through unchanged.
- The published manifest JSON Schema no longer requires `proto` and `from` on
  port forwards, matching the loader defaults (`tcp` and `host`).
- Validation errors for `[qemu] fwd_tunnel_exec`, and for devices attached ad
  hoc through the library or control socket, now name the setting that was
  actually given instead of a fictitious manifest path.
- The resolved manifest (`virtle manifest resolve`) no longer carries the
  unused `MkfsExtraArgs` volume field.
- Releases: `scripts/update-release-nix X.Y.Z` now stamps release versions
  too. When a tag on the tip of `main` still carries the development version
  in `release.nix`, the release workflow stamps it, commits to `main`, and
  moves the tag onto that commit before publishing, so Nix builds of a tag
  report that tag; tags elsewhere with a stale version are still rejected.

## 2026-09-01

- **Breaking library changes noted below.**
- `virtle status` now reports the running VM's lifecycle state and connection
  details.
- `virtle launch` now handles signals and `virtle suspend` with bounded,
  orderly teardown.
- Newly created VM state directories and volume images are private by default
  (`0700` and `0600`).
- Guest and control requests now have bounded memory use and concurrency.
  Oversized control requests return the new `resource_limit` RPC error.

### Library changes

- The library backend contract now exposes `Machine` handles with graceful
  shutdown and live-object capabilities; QEMU is configured directly through
  the exported, zero-value-usable `qemu.Backend` type.
- QEMU machines now stop when their `Start` or `Resume` context is canceled,
  service control-socket suspend requests after library startup, and expose
  guest RPCs only when remote control is configured.
- `vm.Guest.Run` now writes to caller-provided streams and reports non-zero
  command statuses as `*vm.ExitError`; `vm.Output` provides buffered stdout.
- Unit codecs now live in the public `units` package; byte sizes support
  unit-suffixed text, JSON, and TOML round trips. QEMU acceleration and port
  protocols now use typed enums.
- QEMU hotplug now supports complete ad-hoc share, disk, and port-forward
  configurations using the same validation and defaults as manifests.
- `qemu.AccelTCG` now selects a software-emulation CPU and legacy timers on
  x86 microvm guests, so explicitly disabling KVM works on hosts without it.

Construct QEMU with `&qemu.Backend{}` instead of `qemu.New(qemu.Config{})`.
`backend.Instance` is now `backend.Machine`; shutdown and live capabilities
such as suspend, memory resize, and device attach are methods of that machine.
Resume remains a backend capability through `backend.Resumer`. Guest commands
now stream through `GuestCmd` writers and return non-zero status as
`*vm.ExitError`; use `vm.Output` when buffered stdout is more convenient.

Before:

```go
kvm := false
b, err := qemu.New(qemu.Config{
    Machine:       "microvm",
    KVM:           &kvm,
    RemoteControl: qemu.QGA{},
})
inst, err := b.Start(ctx, spec)
defer backend.Shutdown(ctx, inst)

guest, err := inst.RemoteControl()
result, err := guest.Run(ctx, &vm.GuestCmd{Path: "make"})
fmt.Print(result.Stdout)

if s, ok := b.(backend.Suspender); ok {
    err = s.Suspend(ctx, inst, "")
}
```

After:

```go
b := &qemu.Backend{
    MachineType:   "microvm",
    Accel:         qemu.AccelTCG,
    RemoteControl: qemu.QGA{},
}
m, err := b.Start(ctx, spec)
defer m.Shutdown(ctx)

guest, err := m.RemoteControl()
err = guest.Run(ctx, &vm.GuestCmd{Path: "make", Stdout: os.Stdout})

if s, ok := m.(backend.Suspender); ok {
    err = s.Suspend(ctx)
}
```

Additional source migrations: `qemu.Config.Machine` is
`qemu.Backend.MachineType`; `Config.KVM` is the `Backend.Accel` enum;
`vm.Forward.Proto` is a `vm.Proto`; `vm.TermOptions.TERM` is `TermType`; and
setting ownership in `vm.CopyOptions` now requires `Chown: true` alongside
integer `UID` and `GID` fields.

## 2026-08-31

- QEMU guest-agent connections now synchronize the command stream before use,
  preventing stale replies from being mistaken for results such as a
  `guest-exec` PID after reconnecting.

## 2026-08-21

- `workspace.mount_cwd` and other internal guest commands now search standard
  system paths, including the NixOS system profile, instead of depending on the
  QEMU Guest Agent service's restricted `PATH`.

## 2026-08-18 – 2026-08-20

- **v0.3.0 and v0.3.1 released.** Virtle can now be embedded as a Go library:
  callers can construct or load a VM definition, start and control an
  instance, run guest commands, and use optional capabilities such as suspend
  and hotplug. The CLI now runs on the same public interfaces. (#66, #76)

  ```go
  spec, b, err := manifest.Load(r)
  m, err := b.Start(ctx, spec)
  guest, err := m.RemoteControl()
  err = guest.Run(ctx, &vm.GuestCmd{Path: "make", Stdout: os.Stdout})
  ```

- Logging was overhauled: normal runs show warnings, `-v` adds useful lifecycle
  information, and `-vv` adds debugging and background-command output while
  requested SSH and console output remains direct. (#79)

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
