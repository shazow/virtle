# History

Curated newest-first history of breaking changes and important consumer-facing
capabilities, grouped by day. Describe changes from the consumer's perspective,
leading with the affected command or API rather than implementation details.
Keep entries terse. When a day includes both CLI and library changes, group
them by type, CLI first. For compatibility-breaking usage migrations, include
compact before/after examples.

## 2026-09-10

- Manifest templates gain `fromFile "path"`, the file's contents without a
  trailing newline, on every surface; relative paths are the manifest's.
- `[[networks]] type = "virtle"` puts the guest on a network virtle runs in
  userspace (no privilege, no host network changes): a fixed address and MAC
  with a DHCP lease, DNS, `virtle status` reporting the address, and
  `[[networks.forward]]` entries served by virtle rather than QEMU's slirp.
  `type = "tap"` with `tap = "tap0"` hands a host TAP device to the VMM on
  QEMU and Firecracker. `type = "user"` stays the default. See
  [docs/networking.md](docs/networking.md).
- New `[egress]` section for virtle networks: `[[egress.allow]]` and
  `[[egress.deny]]` entries by name pattern, CIDR, or address with optional
  `ports`; everything else is refused before it connects, and loopback,
  link-local, and metadata ranges are always refused. `inspect = true` on an
  allow entry terminates TLS and HTTP to record each request; the guest gets
  the CA certificate at `/etc/virtle/ca.pem`. `[[egress.secrets]]` names a
  secret, a template that renders its value when a request needs it
  (`from = "{{.Env.NAME}}"` or `'{{fromFile "path"}}'`), and the hosts that
  may receive it; the guest gets a token at
  `/etc/virtle/secrets.env` that inspected requests replace with the value.
  See [examples/manifest-sandbox.toml](examples/manifest-sandbox.toml).
- Firecracker manifests accept `[[networks]] type = "tap"`.

### Library changes

- New `vmnet` package: the networking contracts (`Link`, `Network`, `Port`,
  `Egress`, `Flow`, `ErrDenied`) and frame adapters, with the in-process
  gVisor network in `vmnet/userspace` (`userspace.New`, `Network.DialContext`
  and `Listen`, fake-IP DNS) and the standard policy in `vmnet/egress`
  (`Policy` with rules, deny ranges, a `Recorder`, inspection, and `Secret`
  injection; `LoadOrCreateCA`, `GuestEnv`, `GuestFiles`).
- `egress.Policy.Injections` replaces a token the guest writes with a value
  computed as each inspected request passes, or refuses the request that
  carries it (`egress.Injection`; a `Value` returning an error wrapping
  `vmnet.ErrDenied` refuses). A secret is a named injection: its token is
  generated and issued per guest. `egress.Policy.Admit` decides on every
  inspected request before any token is replaced. Values are read only when
  a request carries the token, and `egress.Event` records refusals and the
  injections applied.
- `qemu.Backend` gains `Network vmnet.Network` and `Link` (`qemu.User`,
  `qemu.TAP`, `qemu.Stream`); `firecracker.Backend` gains `Link`
  (`firecracker.TAP`). A pair that cannot work fails `Start` with an error
  wrapping `errors.ErrUnsupported`. With a `Network`, `Spec.Ports` and
  `Attach(vm.Forward)` are exposed on the network port instead of slirp
  forwards and hotplugged NICs, `Detach` removes them, and a suspended
  machine resumes with its address and MAC.
- `backend.Status` gains `Networks []backend.NetworkStatus` (ID, MAC,
  whether the NIC is attached to a virtle network, and its address there).
- `vm.Spec` gains `Egress *vm.Egress` (`Allow`, `Deny []vm.Reach`,
  `Secrets []string`): the guest's own policy, which only narrows the
  network's.
- `manifest.Load` builds the network and policy a manifest declares; the
  returned `*qemu.Backend` owns them and implements `io.Closer`.

## 2026-09-09

- `backend = "firecracker"` launches a Firecracker microVM instead of QEMU:
  direct kernel boot, raw disks, serial output, and the usual `virtle launch`,
  `status`, and `rpc` lifecycle. Linux with KVM only; guest control, SSH,
  networking, shares, suspend, balloon, and hotplug stay QEMU-only and fail
  validation. See [docs/firecracker.md](docs/firecracker.md).
- `[[mounts]] type = "image"` gains `target = "/"`, naming the root device on
  both backends: virtle passes `root=/dev/vdX` and `ro`/`rw` for it, and
  `kernel.initrd_path` is optional when a boot names its root device (or
  carries its own `root=`). Nothing is picked automatically on either
  backend: a disk boot without an initrd must set `target = "/"` on its
  root image or pass `root=`, and fails validation otherwise.
- QEMU manifests can set `vsock.enabled = false` for guests that do not use
  host-guest vsock, dropping the `/dev/vhost-vsock` requirement.
- `virtle launch` lets `Machine.Shutdown` stop the guest gracefully on
  SIGINT/SIGTERM before canceling the machine, and drains accepted
  wait/kill/shutdown/suspend RPC responses before exiting. `^Z` (SIGTSTP) on
  a backend that cannot suspend is ignored with a warning instead of shutting
  the VM down.
- `nix flake check` gains real-KVM end-to-end checks that boot both backends
  on a shared tiny kernel, through the CLI and through the Go API (the
  `backendtest` contract, root and scratch disks, the console); they need a
  `kvm` builder (see CONTRIBUTING.md). `nix run .#benchmark-backends`
  compares the backends.

### Library changes

- New `backend/firecracker` package: `&firecracker.Backend{}` implements
  `backend.Backend`, and its machines implement `backend.StatusReporter` and
  `backend.ConsoleProvider`, with the same `vm.Spec` and `backend.Machine`
  as QEMU. Spec features it cannot honor fail `Start` with an error wrapping
  `errors.ErrUnsupported`.
- `vm.Disk{GuestPath: "/"}` names the root device on both backends (virtle
  passes `root=` for it). Any other `GuestPath` needs a guest agent and now
  fails `Start` with an error wrapping `errors.ErrUnsupported` on both
  backends; QEMU used to ignore it silently.
- `vm.Disk.Size` (manifest `image.create` + `image.size`) creates a missing
  raw ext4 image on Firecracker too, with QEMU's 256 MiB minimum.
- `backend.ConsoleProvider` is implemented by QEMU and Firecracker machines
  whose console is `print`: `Machine.Console` returns a `vm.Term` over the
  guest's serial port that replays recent output before live output, so
  readiness detection and driving a console shell need only `bufio` and
  `io`; without a print console it reports `errors.ErrUnsupported`. A
  session whose reader falls 1 MiB behind is dropped with an error wrapping
  `vm.ErrTermFellBehind` and a warning on the backend's `Logger`, so a
  stalled consumer never stalls the guest.
- `vm.Disk.ReadOnly` is honored by both backends and by QEMU hotplug.
  **Breaking:** a `vm.Disk` that replaces a manifest disk must now set
  `ReadOnly: true` itself to keep a read-only mount:
  ```go
  // Before: the manifest's read_only = true survived the overlay.
  spec.Disks[0] = vm.Disk{Path: "rootfs.img"}
  // After: the Spec entry is the whole truth.
  spec.Disks[0] = vm.Disk{Path: "rootfs.img", ReadOnly: true}
  ```
- **Breaking:** an empty `vm.Spec.Dir` now means the process working
  directory on both backends, as for `exec.Cmd.Dir`: relative kernel, disk,
  and share paths resolve there, and runtime state goes to a private
  temporary directory that is removed when the machine exits. QEMU used to
  work in a never-removed temporary directory, so relative Spec paths did
  not resolve against the caller's directory. `Suspend` and `Resume` need a
  `Dir`, since saved state lives in its `.virtle`:
  ```go
  // Before: state landed in a temporary directory that outlived the machine.
  m, err := b.Start(ctx, &vm.Spec{Kernel: vm.Kernel{Path: "vmlinuz"}})
  // After: set Dir to keep state across runs (and to Suspend/Resume).
  m, err := b.Start(ctx, &vm.Spec{Dir: dir, Kernel: vm.Kernel{Path: "vmlinuz"}})
  ```
- A zero `vm.Spec.CPUs` selects the host CPU count on Firecracker too
  (within its limit of 32), matching QEMU. Small guests should set `CPUs`
  and `Memory` explicitly, as the test fixtures do.
- `qemu.Backend.DisableVSock` is the Go counterpart of `vsock.enabled = false`.
- The deprecated `backend.Shutdown` helper is gone; call `Machine.Shutdown`.
- **Breaking:** `backend/qemu/session` no longer exports `Run`, `Options`,
  and `ExitCode`; the CLI foreground loop is backend-neutral and lives in
  `internal/session`. Programs that embedded it should drive
  `backend.Machine` directly (`Start`, `Wait`, `Shutdown`, `Console`) or run
  the `virtle launch` command.

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
