# Fast shared guest and backend comparison

From the repository root, build and exercise **both real KVM backends through
the public virtle CLI**:

```sh
nix build path:.#checks.x86_64-linux.e2e-fast --no-link -L
nix run path:.#benchmark-backends -- --output benchmark-results/fast \
  --pairs 10 --warmup-pairs 2
```

`path:.` includes untracked working-tree files. These commands use the root
`flake.lock`; there is no nested flake or lock file to generate. The first build
compiles a custom Linux kernel and can take several minutes. Build the runner
before timing on an otherwise idle host.

Use a new output directory for every run; the runner refuses to overwrite one.
Each pair runs both backends, alternating FC/QEMU then QEMU/FC. Pair counts must
be even so each backend starts equally often. Warmups use the same alternating
order, remain in the raw data, and are excluded from summaries. The default is
two warmup trials and ten measured trials **per backend**. The flake check uses
two measured trials per backend and no warmups; it checks correctness without
any performance ratio or timing threshold beyond a generous hang timeout.
Both default to `--teardown kill`, applying the same fast lifecycle RPC to both
backends so test cleanup does not wait for an unavailable guest agent. Pass
`--teardown shutdown` when specifically studying each backend's graceful
shutdown policy; that mode is intentionally not latency-equivalent.

Linux x86_64 and accessible `/dev/kvm` are required. The runner opens KVM and
checks its API version; missing or denied KVM fails the run. QEMU explicitly
uses `accel=kvm` with no TCG fallback. The flake check declares
`requiredSystemFeatures = [ "kvm" ]`, so the Nix builder must advertise `kvm`
and expose the device in its sandbox. An unsuitable builder cannot satisfy this
check; it never returns a successful skip. The QEMU integration and Firecracker
raw-disk recipe checks remain separate.

## What the fixture contains

`fixtures/fast/kernel.nix` starts from `tinyconfig` in the root-pinned nixpkgs
Linux source. Serial console, KVM guest support, virtio MMIO/block/console/net,
IPv4 (no IPv6), packet sockets, ext4, and the i8042 keyboard shutdown path are
built in. Modules, PCI and ACPI are disabled. Gzip replaces tinyconfig's XZ kernel compression to reduce
QEMU's decompression cost. The same kernel build supplies ELF `vmlinux` to
Firecracker and `bzImage` to QEMU; the formats differ because their loaders
differ.

Two package outputs keep capability scope explicit:

- `e2e-fast-fixture` is the minimal backend benchmark. Its kernel gained the
  virtio-net NIC and IPv4 with the virtle network, so timings before that
  change are not comparable with timings after it.
- `e2e-fast-userspace-fixture` adds built-in eventfd, inotify, file locking and
  Unix-domain sockets. These common primitives support Go, libuv and similar
  Nix closures without adopting a distribution kernel's PCI, ACPI, modules,
  device drivers, filesystems, cgroups or namespaces. It reuses the same
  initramfs and manifests; the minimal profile remains the benchmark baseline.
  No check boots it yet: `nix flake check` only generates its kernel
  configuration (`e2e-fast-userspace-config`), so the fragment stays valid.

Both use the **identical initramfs**, 1 vCPU and 128 MiB RAM. The initramfs is
the root filesystem: a static BusyBox, small init scripts and an input file
containing `21`. Init mounts devtmpfs/proc/sysfs; the workload reads the input,
doubles it, writes and reads back `/tmp/result`, verifies `42`, and prints the
complete line `VIRTLE_READY:42`. When the guest has a NIC (the Go API network
scenarios attach one; the CLI manifests attach none), it first takes a DHCP
lease with `udhcpc`, prints `VIRTLE_NET:<address>`, serves a TCP echo on port
7, with `virtle.egress=PORT` on the command line connects to `allowed.test`
and `blocked.test` on that port and prints the outcome, and with
`virtle.inject=PORT` fetches `http://inject.test:PORT/echo` with
`$VIRTLE_RANDOM$` in a header and the query, then with `$VIRTLE_REJECT$`, then
`/forbidden`, and prints what came back each time. There
is no modprobe, NixOS activation, service manager, SSH, or guest agent. No disk
is attached in the CLI benchmark; the Firecracker recipe covers raw disk I/O and
clean unmounting. The archive normalizes owner, timestamps, ordering, inode
numbering and gzip headers. All dependencies come from the root lock.

QEMU runs its `microvm` machine with KVM, qboot, and PCIe, ACPI, PIT, PIC, RTC,
USB and option ROMs disabled. It retains virtle's normal console and control
device setup; `vsock.enabled = false` avoids attaching an unused vhost-vsock
device or requiring `/dev/vhost-vsock` in the Nix sandbox. Firecracker runs
through its normal API configuration. See QEMU's
[microvm documentation](https://www.qemu.org/docs/master/system/i386/microvm.html).

## Timing and interpretation

Results are **directional, non-publication-grade**. A small common kernel and
userspace remove most distribution boot and module-loading work, making VMM
and virtle startup costs easier to see. This measures the public CLI
experience, not a pure VMM hardware boot time:

- `process_to_ready_seconds`: host monotonic time immediately before spawning
  `virtle launch` to the reader observing the complete guest readiness line.
  Includes CLI loading, backend setup, VMM startup, kernel loading/boot, guest
  work and console delivery. Status queries are outside this interval.
- `teardown_seconds`: immediately before spawning the selected lifecycle RPC
  (`kill` by default, or `shutdown` when requested) until the RPC returns and
  the foreground launch process is reaped. Includes the backend's existing
  shutdown policy and cleanup; it is reported separately.

With the default `kill` teardown, both backends take the same hard-stop path and
the suite avoids QEMU's guest-agent timeout. Shutdown policies differ when
`--teardown shutdown` is selected: Firecracker injects Ctrl-Alt-Del; BusyBox
init runs the shutdown script, prints `VIRTLE_SHUTDOWN:done` and resets the
guest. QEMU's CLI backend first probes for QGA, which this fixture does not
run, then quits through QMP, so its guest-agent timeout is included and a QEMU
guest shutdown marker is not required. These numbers do not compare equivalent
guest shutdown protocols.

Trials require a ready status, live VMM PID and control/monitor sockets,
successful status/lifecycle commands and the expected launch exit, a gone VMM
PID, and removal of all runtime sockets and temporary runtime directories. In
`shutdown` mode, Firecracker must also emit its guest shutdown marker.
Validation occurs before deleting each trial's working directory. Persistent
lock files are allowed. Failures abort the run and retain diagnostics; no
failed trial can produce an overall success.

`results.json` contains host/version/hash metadata, trial order, all successful
and failed trial records, and min/median/max summaries. Each trial directory
also contains `trial.json` and `console.log`. The Nix check keeps only a
deterministic success marker after validating the same data; use the benchmark
app when raw artifacts are needed. Host scheduling, caches, CPU frequency,
loader differences, virtle monitor readiness and normal device defaults still
affect measurements. There is no CPU isolation, cold-cache control, statistical
confidence interval, or asserted performance ratio.

## Reuse

Build all artifacts for an interactive CLI launch:

```sh
nix build path:.#e2e-fast-fixture -o result-fast-fixture
nix run path:. -- --manifest "$PWD/result-fast-fixture/firecracker.toml" launch
# In another terminal in the same directory:
nix run path:. -- --manifest "$PWD/result-fast-fixture/firecracker.toml" status
nix run path:. -- --manifest "$PWD/result-fast-fixture/firecracker.toml" rpc shutdown
```

Use `qemu.toml` for QEMU, or `e2e-fast-userspace-fixture` for the
userspace-capable kernel. The fixture output also exposes `vmlinux`, `bzImage`,
`kernel.config`, `initrd`, `rootfs.ext4` (the same guest tree as a raw root
disk, for boots without an initrd), and `fixture.json`. Its Nix passthru
attributes are `kernel`, `initrd`, `rootfs`, `firecracker`, and `qemu`.

The `e2e-api` flake check boots the same fixture through the public Go API
instead of the CLI: the Go tests in this directory (build tag `integration`)
run the backend conformance suite from `backend/backendtest` against both
backends and cover the `vm.Spec.Dir` contract, booting from the root disk, a
scratch disk the host reads back, and the serial console. They read
`VIRTLE_E2E_FIXTURE`, `VIRTLE_E2E_QEMU`, and `VIRTLE_E2E_FIRECRACKER` and skip
without them. Other E2E derivations can import
`fixtures/fast` with `{ inherit pkgs; workload = ./my-ready-script; }` to run a
different BusyBox workload on the same kernel. Keep the readiness protocol
when reusing this runner. Tests needing disks or guest agents can reuse the
kernel and grow their own initramfs/manifests beside this fixture, leaving the
baseline benchmark stable. Kernel changes are centralized in `kernel.nix`.

The runner's own ordering and summary accounting is unit-tested with in-memory
data (`python3 -m unittest discover -s tests/e2e -v`, or the `e2e-runner` flake
check); actual guest readiness, status and cleanup are tested by the KVM check.
