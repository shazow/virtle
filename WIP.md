# Firecracker implementation status

The requested backend, common manifest/CLI integration, focused tests, Nix
guest/check, recipe, README, and history updates are implemented for the
capability set below. Full QEMU feature parity is **not** implemented.
Validation results and the remaining environment/procedural limits are recorded
below.

## Implemented and compatibility decisions

- `backend = "qemu"` remains the default; `backend = "firecracker"` selects the
  public `backend/firecracker.Backend`. Both use `vm.Spec`, `backend.Machine`,
  TOML/JSON loading, the common foreground loop, and lifecycle control RPCs.
- Firecracker boots a host-architecture kernel, optional initrd, and existing
  raw disks (first disk is root). It defaults to one vCPU and 1024 MiB, accepts
  1–32 vCPUs, and passes the caller's kernel command line to the API. Firecracker
  appends `root=/dev/vda` and `ro`/`rw` for the first disk; a conflicting root
  such as `root=/dev/vda1` is currently unsupported. API boot acceptance
  is explicitly distinguished from guest workload readiness.
- Runtime API paths are unique private directories; state uses a non-following
  lock-file open and the same VM-name lock as QEMU. Existing control socket paths
  are never deleted to make room. Startup errors, cancellation, shutdown
  deadlines, and repeated/concurrent teardown are covered. Child processes run
  in their own process group; completion follows reaping, owned-path cleanup,
  and delivery of accepted control responses (bounded writes for stalled peers).
  Stderr and HTTP diagnostics are bounded and quoted. API bodies and headers are
  bounded, requests have deadlines, and redirects/proxies are disabled.
- QEMU SSH, suspend/resume, readiness, exit codes, and helpers retain their
  existing adapter. The shared session now separates startup cancellation from
  foreground-owned graceful shutdown for both backends.
- `vm.Disk.ReadOnly` now carries manifest read-only policy into both backends
  and QEMU hotplug. Replacing a manifest disk in Go must explicitly retain
  `ReadOnly: true`; false now overrides it. QEMU manifest-only launches preserve
  their policy. This intentional source behavior change is in HISTORY.md.
- Control status keeps existing JSON wire names: `qmpSocket` is the Firecracker
  API socket, `qmpReadyAt` is API setup completion. No protocol rename is needed
  to keep existing control clients working.
- Input paths remain host paths when handed to an external VMM. They cannot
  usefully be replaced with Go readers in the Firecracker API. Manifest input
  remains reader-based; pure configuration tests avoid the filesystem. Real
  filesystem tests are confined to the actual process/socket/lock boundary.

## Known Firecracker limitations and decisions still needed

These features are unsupported and, when configurable through the manifest or
Spec, rejected rather than silently approximated:

| Feature / compatibility point | Current limitation and next design decision |
| --- | --- |
| Guest control, SSH, guest files, workspace sharing | No QGA equivalent is assumed. `RemoteControl` wraps `errors.ErrUnsupported`. Select a guest protocol/agent and transport before adding command/file APIs and readiness signaling. |
| Networking and vsock | No TAP provisioning, user networking, port forwarding, or vsock device. Explicit SSH/readiness and vsock sections, including default/zero CID ranges, are rejected; omitted decoder defaults are accepted. Decide a neutral network API and host ownership/privilege model, plus Firecracker's Unix-vsock transport mapping. |
| Interactive console / graphics | Only `serial = "off"` or `"print"`; guest console parameters are supplied explicitly. Choose terminal ownership and resize semantics before adding interactive input. |
| Suspend, snapshots, balloon, hotplug | Optional capability interfaces are not advertised by the local Firecracker machine. Define snapshot compatibility and memory/device policy before implementing them. The existing RPC client has a fixed Go method set but returns `ErrUnsupported` for unadvertised server methods. |
| Host helpers and notification hooks | Rejected for Firecracker. The QEMU launch planner still owns these; further extraction is needed for equivalent Firecracker workflows. |
| Disk creation, qcow2, disk cache/serial settings | Raw existing files only; no automatic creation/formatting/mounting. Choose common volume provisioning separately from VMM device configuration. |
| Arm64 clean shutdown | Firecracker's `SendCtrlAltDel` is x86-only. Arm64 boot code builds, but host-initiated graceful shutdown needs a guest transport; today API rejection is reported and the process is killed. No arm64 guest boot was run on this amd64 host. |
| Jailer, cgroups, privilege dropping, namespaces | Direct Firecracker launch with its default seccomp policy. Host multi-tenant isolation is outside this backend's current scope; decide whether/how virtle owns the jailer and resource policy before making stronger isolation claims. |
| Abrupt host/launcher death | Normal exits/signals are managed. SIGKILL/host crashes can leave a VMM or runtime paths; no crash-recovery daemon or parent-death policy is implemented. Inspect any remaining process before removing a stale control socket. Lock files are persistent and harmless once unlocked. |
| Non-Linux / missing KVM / cross-architecture emulation | Unsupported by Firecracker. No fake-success check or TCG fallback. |
| API startup notification | Firecracker has no inherited readiness descriptor. Only socket connection establishment is retried on a cancellable 10ms timer. API mutations are not retried. Tests use channels/events and deadlines, with no timed sleeps. |
| Root flake versus recipe lock | The root check uses the repository's existing nixpkgs lock. Before this change is published, use the documented `--override-input virtle path:. --no-write-lock-file` recipe command; otherwise the recipe's remote virtle input may predate Firecracker support. |

## Test-first record

Focused red runs preceded implementation of manifest selection, API requests,
configuration and validation, process startup/rollback, control sockets,
startup diagnostics, ephemeral state, common session dispatch and cancellation,
disk read-only propagation, and shared state locking. Representative commands:

```sh
go test ./internal/manifest -run TestBackendSelection -count=1
go test ./backend/firecracker -run TestAPI -count=1
go test ./backend/firecracker -run 'TestConfiguration|TestSpecValidation' -count=1
go test ./backend/firecracker -run 'TestLifecycle|TestStartup|TestCancellation|TestPrivateState' -count=1
go test ./backend/firecracker -run TestControl -count=1
go test ./manifest ./backend/qemu -run 'TestLoadBackend|TestDiskReadOnly' -count=1
go test ./internal/session -count=1
go test ./internal/session -run TestSessionOwnsGracefulShutdown -count=1
go test ./internal/manifest -run TestFirecrackerCompatibilityValidation -count=1
go test ./backend/firecracker -run 'TestStartupDiagnostics|TestConcurrentShutdown|TestShutdownTimeout|TestDefaultState' -count=1
go test ./backend/firecracker -run TestSharesManifestLockWithQEMU -count=1
go test ./backend/firecracker -run TestEarlyProcessExitPreservesStatus -count=1
go test ./internal/session -run TestSessionReportsStartup -count=1
go test ./backend/qemu/internal/vmm -run TestAdHocHotplugDevicesReceiveExecutablePlansAndDefaults -count=1
```

The real Nix check initially failed because the output did not yet exist, then
booted and computed `42` but failed shutdown. Real boot debugging identified
missing i8042/AT keyboard modules, BusyBox module-option handling, and x86
`poweroff` versus reset semantics. The guest now supplies explicit keyboard
module options, retains ACPI IRQ discovery, and resets only after unmounting.
The standalone real Firecracker integration test now passes in 1.56 seconds.

TDD process limitation: several later affirmative regression cases (manifest
overlay, concurrent shutdown, and configured shutdown timeout) passed on their
first run because the related behavior had already been implemented under
earlier tests. They were not artificially broken to manufacture a red run.
Thus the literal requirement that every individual behavior have its own
observed failing test before implementation was not completely met; the major
implementation steps and the real guest fixes did follow red/green cycles.

## Validation commands and results

- Initial `go test ./...` under this shell's `umask 0077`: failed existing QEMU
  `TestEnsureDirectoryCreatesPrivateAndPreservesExistingMode` and
  `TestGuestDirectoryInstallScriptLeavesExistingDirsUntouched` (0700 vs 0755).
  These failures were reproduced before implementation. No unrelated tests
  were weakened; `(umask 022; go test ./...)` passes the whole suite.
- `go test -race ./backend/firecracker ./internal/session ./backend/qemu/session ./manifest ./internal/control`:
  passed (including the child-process protocol and shutdown concurrency tests).
- `go test ./backend/firecracker -run 'TestConcurrentShutdown|TestLifecycle|TestControlSocket' -count=20`:
  passed after fixing a fake-server race: the test child now waits for HTTP
  handlers to flush before exiting.
- `gofmt -w` on all changed Go files and `go run . manifest schema > manifest.schema.json`:
  completed. The generated schema freshness test passes.
- `/nix/store/sgijssgx5ylh2vhajwp98f9sbhsdwhjy-nixfmt-1.3.1/bin/nixfmt flake.nix docs/recipes/firecracker/flake.nix docs/recipes/firecracker/guest.nix`:
  completed with the repository's pinned formatter; the same command with
  `--check` also passed.
- `nix flake check path:. --no-build --all-systems`: passed, including both
  Linux architectures and the x86_64-only Firecracker check requirement.
- `nix run path:./docs/recipes/firecracker --override-input virtle path:. --no-write-lock-file`:
  passed; the documented recipe app performed the guest computation, graceful
  shutdown and cleanup using the local implementation.
- `nix eval path:./docs/recipes/firecracker#packages.x86_64-linux.manifest.drvPath --override-input virtle path:. --no-write-lock-file`:
  passed; no lockfile was changed.
- `nix build path:.#checks.x86_64-linux.firecracker --no-link -L`: passed.
  It booted through the CLI, observed `VIRTLE_READY:42`, requested shutdown over
  the control socket, observed `VIRTLE_SHUTDOWN:unmounted`, and verified exit 0
  and socket cleanup. Tested with Firecracker 1.15.1 and Linux 6.18.35.
- `nix build path:.#checks.x86_64-linux.integration --no-link -L`: blocked.
  The outer vmTools QEMU ran at approximately 100% CPU without producing test
  results; the build was interrupted after about 11 minutes. Its last visible
  messages were virtiofsd connections, including nonfatal file-handle permission
  warnings. This does not verify the QEMU flake check.
- `(umask 022; go test -tags integration ./backend/qemu/internal/launch -run TestIntegration -count=1)`:
  passed using this host's `/bin/sh`; the Nix wrapper's dash-specific run remains
  unverified.
- With the pinned QEMU/kernel/initrd below,
  `go test -tags integration ./backend/qemu -run TestIntegrationBackend -count=1 -timeout 3m -v`
  timed out in the guest subtest. An untouched `git archive HEAD` checkout run
  with `-run '^TestIntegrationBackend/guest$' -timeout 60s` also timed out. Thus
  the same guest fixture fails without this implementation; no claim is made
  that QEMU's real guest operations were successfully validated here. Remaining
  decision: diagnose/update the existing QEMU fixture or validate it on another
  host before relying on that integration check. The untouched guest run was blocked
  in QGA `socketClient.synchronize` / `readDelimited` when its timeout fired.
- `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./...` and
  `GOOS=darwin GOARCH=arm64 go build ./...`: passed. An initial Linux arm64
  cross-build with cgo enabled failed because the host assembler cannot assemble
  aarch64 instructions; disabling cgo matches the repository's Nix build.

## Independent-review blocker fixes

Each implementation fix followed an observed failing regression: completion
before a blocked control response write, Firecracker schema validation without
an initrd, explicit TOML/JSON readiness and vsock settings, and the rootfs
derivation's imported inode ownership check. All four then passed. The final
completion test uses `net.Pipe` and `testing/synctest` to force the race without
sleeps; concurrent wait/shutdown/kill RPCs also complete and deliver responses.

The rootfs-only command
`nix build path:./docs/recipes/firecracker#rootfs --override-input virtle path:. --no-write-lock-file --no-link -L`
failed with the inode check before normalization, then passed. Adding
`--rebuild` passed the output comparison. `/`, `/lost+found`, and `/input` are
checked as UID/GID 0 inside the derivation. Reproducibility is verified for
this rootfs output, not for the entire guest closure.

`go test ./...` with `umask 022` passed on rerun. Its first run hit the existing
`TestServerBoundsConcurrentHandlers` broken-pipe race: a rejected connection
can close before the client writes its request. A Go source overlay using the
unchanged staged `internal/control/server.go` reproduced it with
`go test -overlay /tmp/virtle-control-baseline-jhalfxr5/overlay.json ./internal/control -run '^TestServerBoundsConcurrentHandlers$' -count=1000 -cpu=1,2,4,8`;
the current server reproduces it under the same stress. That independent
handler-limit behavior remains outside this fix's scope.

Focused manifest/schema/backend/control tests, focused race tests,
`go vet ./...`, `gofmt`, the pinned Nix formatter check for all three changed
Nix files, schema regeneration/freshness, and
`nix flake check path:. --no-build --all-systems` passed. After the final
lifecycle fixes, `nix build path:.#checks.x86_64-linux.firecracker --no-link -L`
was rerun successfully: the guest computed `42`, unmounted, shut down cleanly,
exited zero, and left no runtime sockets.

## Same-guest Firecracker/QEMU comparison

After the implementation passed its checks, the same minimal workload was run
three times per backend in counterbalanced order. Both used one vCPU, 256 MiB,
the Linux 6.18.35 package, the identical initrd, and the identical read-only raw
ext4 disk; QEMU used that kernel package's `bzImage`, while Firecracker required
its ELF `vmlinux`. Every run reached `VIRTLE_READY:42`, returned status, accepted
`virtle rpc shutdown`, exited 0, and removed its control socket. No VMM process
remained afterward.

- Firecracker median process-start-to-ready: 2.765s.
- QEMU median process-start-to-ready: 3.210s (0.445s / 16.1% slower in this
  three-trial directional sample).
- Firecracker median shutdown RPC-to-exit: 0.073s; all three runs delivered
  Ctrl-Alt-Del to the guest and observed `VIRTLE_SHUTDOWN:unmounted`.
- QEMU median shutdown RPC-to-exit: 15.064s; the guest had no QGA, so all three
  runs reached the configured agent-less shutdown wait before QMP exit and did
  not execute the guest unmount handler. QEMU's persistent state directory
  remained by design; Firecracker removed its separate private API directory.

These are functional smoke trials with warm caches and no CPU pinning, not a
publication-grade performance benchmark. The main design result is that the same
guest workload and disk run on both backends, while clean agent-less shutdown is
currently materially better on the implemented x86 Firecracker path.

The QEMU attempts used:

```sh
PATH=/nix/store/4i1mpm6sfhfhrmkrf1banfsqbsrnm223-qemu-11.0.0/bin:$PATH
VIRTLE_INTEGRATION_KERNEL=/nix/store/p0z76jwkwqznh9lx5gpcriybxc5x0r0m-linux-6.18.35/bzImage
VIRTLE_INTEGRATION_INITRD=/nix/store/n865jf7bl1fy1h62705fj4y1j6nq74dd-virtle-integration-initrd/initrd
VIRTLE_INTEGRATION_MACHINE=microvm
```

These were environment assignments on the test command. QEMU children left by
Go's hard test timeouts were explicitly terminated afterward.

The actual Go/KVM integration run (no skip) used this exact environment:

```sh
VIRTLE_FIRECRACKER_BINARY=/nix/store/5n34vw4asnnpfdzr8i4fh48cjs41p5c6-firecracker-1.15.1/bin/firecracker \
VIRTLE_FIRECRACKER_KERNEL=/nix/store/4cw4mh9ip96sz33nba7sl99mxmvlnxzy-linux-6.18.35-dev/vmlinux \
VIRTLE_FIRECRACKER_INITRD=/nix/store/9ypfi6v8qjzy15bbd0w7i39dr3rvdfxf-virtle-firecracker-initrd/initrd \
VIRTLE_FIRECRACKER_ROOTFS=/nix/store/p6887j6wjq5bcdfzaicsha1760fizm9z-virtle-firecracker-rootfs/rootfs.ext4 \
go test -tags integration ./backend/firecracker -run TestIntegrationFirecracker -count=1 -v
```

Result: PASS. The fixture guest operation and graceful shutdown both ran.
