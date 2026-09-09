"""Exercise the public CLI against the shared fast guest; keep diagnostics."""

import argparse
import contextlib
from dataclasses import dataclass
import fcntl
import hashlib
import json
import os
from pathlib import Path
import platform
import select
import signal
import statistics
import subprocess
import sys
import tempfile
import threading
import time


READY = b"VIRTLE_READY:42"
SHUTDOWN = b"VIRTLE_SHUTDOWN:done"
TIMEOUT = 30
STATUS_RETRY_INTERVAL = 0.05
CLEANUP_RETRY_INTERVAL = 0.05
OUTPUT_LIMIT = 1024 * 1024
LABEL = "Directional, non-publication-grade; includes virtle, host scheduling and guest boot."


def schedule(pairs, warmup_pairs):
    for warmup, count in ((True, warmup_pairs), (False, pairs)):
        for pair in range(count):
            order = (
                ("firecracker", "qemu") if pair % 2 == 0 else ("qemu", "firecracker")
            )
            for position, backend in enumerate(order):
                yield {
                    "backend": backend,
                    "warmup": warmup,
                    "pair": pair,
                    "position": position,
                }


def summarize(rows):
    summary = {}
    for backend in ("firecracker", "qemu"):
        measured = [
            r
            for r in rows
            if r["backend"] == backend and not r["warmup"] and r["passed"]
        ]
        if not measured:
            continue
        summary[backend] = {"trials": len(measured)}
        for metric in ("process_to_ready_seconds", "teardown_seconds"):
            values = [r[metric] for r in measured]
            summary[backend][metric] = {
                "min": min(values),
                "median": statistics.median(values),
                "max": max(values),
            }
    return summary


def require_kvm():
    # Check actual KVM access, not just permission bits; never fall back to TCG.
    with open("/dev/kvm", "rb+") as device:
        if fcntl.ioctl(device, 0xAE00, 0) != 12:  # KVM_GET_API_VERSION
            raise RuntimeError("KVM API version 12 is required")


def teardown_rpc(mode):
    if mode not in ("kill", "shutdown"):
        raise ValueError(f"unsupported teardown mode: {mode}")
    return mode


def launch_exit_ok(teardown, code):
    # A successful hard-stop RPC intentionally makes the foreground CLI report
    # its killed VMM as an error. Graceful shutdown remains a clean exit.
    return code == (1 if teardown == "kill" else 0)


def wait_for_status(probe, *, timeout=TIMEOUT, clock=time.monotonic, sleep=time.sleep):
    """Retry control status within one deadline, including subprocess time."""
    deadline = clock() + timeout
    last_error = None
    while (remaining := deadline - clock()) > 0:
        try:
            return probe(remaining)
        except (subprocess.CalledProcessError, subprocess.TimeoutExpired) as error:
            last_error = error
        remaining = deadline - clock()
        if remaining > 0:
            sleep(min(STATUS_RETRY_INTERVAL, remaining))
    raise TimeoutError(f"control status unavailable after {timeout} seconds") from last_error


@dataclass(frozen=True)
class ProcessStat:
    pgid: int
    sid: int
    start: int
    alive: bool


def process_stat(text):
    # comm can itself contain spaces and parentheses. Fields start at state (3).
    fields = text[text.rindex(")") + 1:].split()
    return ProcessStat(
        int(fields[2]), int(fields[3]), int(fields[19]),
        fields[0] not in ("Z", "X") or int(fields[17]) > 1,
    )


def read_process(pid):
    try:
        return process_stat(Path(f"/proc/{pid}/stat").read_text())
    except (FileNotFoundError, ProcessLookupError):
        return None


class OwnedVMM:
    """Retain group ownership through launcher exit; signal only pinned members."""

    def __init__(self, pid, sid):
        self.pid = self.pgid = pid
        self.sid = sid
        self.members = {}  # PID -> (pidfd, process start time)

    @classmethod
    def capture(cls, pid, launcher_pid):
        if type(pid) is not int or pid <= 1 or pid in (launcher_pid, os.getpid()):
            raise RuntimeError("status does not identify an owned VMM group")
        owned = cls(pid, launcher_pid)
        try:
            owned.members[pid] = owned._open(pid)
            owned.start = owned.members[pid][1]
            owned._refresh()
            return owned
        except BaseException:
            owned.close()
            raise

    @classmethod
    def capture_launcher(cls, launcher_pid):
        """Pin Popen's new session before any wait/poll can reap its leader."""
        if type(launcher_pid) is not int or launcher_pid <= 1 or launcher_pid == os.getpid():
            raise RuntimeError("launcher does not identify an owned VMM session")
        owned = cls(launcher_pid, launcher_pid)
        try:
            owned.members[launcher_pid] = owned._open(launcher_pid)
            owned.start = owned.members[launcher_pid][1]
            # Include all groups in this session, including a VMM whose group
            # leader has already exited. The unreaped launcher (even a zombie)
            # anchors discovery until failure cleanup or success validation ends.
            owned.pgid = None
            return owned
        except BaseException:
            owned.close()
            raise

    def launcher_alive(self):
        stat = self._matches(self.sid, self.members[self.sid])
        return stat is not None and stat.alive

    def wait_launcher(self, guest, *, timeout):
        # A pidfd alone does not reserve the numeric PID/session after reaping.
        # Wait without reaping so even descendants first seen during failure
        # cleanup can still be tied to this exact launcher's session.
        fd, _ = self.members[self.sid]
        if not select.select([fd], [], [], timeout)[0]:
            raise subprocess.TimeoutExpired(guest.args, timeout)
        status = os.waitid(os.P_PID, self.sid, os.WEXITED | os.WNOWAIT)
        return status.si_status if status.si_code == os.CLD_EXITED else -status.si_status

    def validate_status(self, pid):
        if type(pid) is not int or pid <= 1 or pid in (self.sid, os.getpid()):
            raise RuntimeError("status does not identify an owned VMM group")
        self._refresh()
        member = self.members.get(pid)
        stat = self._matches(pid, member) if member is not None else None
        if stat is None or stat.pgid != pid:
            raise RuntimeError("status does not identify an owned VMM group")

    def _contains(self, stat):
        return stat.sid == self.sid and (self.pgid is None or stat.pgid == self.pgid)

    def _open(self, pid):
        fd = os.pidfd_open(pid)
        try:
            stat = read_process(pid)
            if stat is None or not self._contains(stat):
                raise RuntimeError("status does not identify an owned VMM group")
            # Check after reading procfs: this exact process must still exist,
            # so the stat cannot belong to a replacement of the numeric PID.
            signal.pidfd_send_signal(fd, 0)
            return fd, stat.start
        except BaseException:
            os.close(fd)
            raise

    def _matches(self, pid, member):
        fd, start = member
        stat = read_process(pid)
        if stat is None or not self._contains(stat) or stat.start != start:
            return None
        try:
            signal.pidfd_send_signal(fd, 0)
        except ProcessLookupError:
            return None
        return stat

    def _refresh(self):
        group = {}
        for entry in Path("/proc").iterdir():
            if entry.name.isdecimal():
                pid = int(entry.name)
                stat = read_process(pid)
                if stat is not None and self._contains(stat):
                    group[pid] = stat
        # A retained member must still anchor this PGID/session before we adopt
        # new descendants. A replacement leader means our old group is gone.
        retained = list(self.members.items())
        if not any(self._matches(pid, member) for pid, member in retained):
            leader = group.get(self.pid)
            if any(stat.alive for stat in group.values()) and not (leader and leader.start != self.start):
                raise RuntimeError("cannot verify ownership of surviving VMM process group")
            return []
        added = {}
        try:
            for pid in group.keys() - self.members.keys():
                try:
                    added[pid] = self._open(pid)
                except ProcessLookupError:
                    continue
            # Recheck after opening descriptors, before trusting new members.
            if added and not any(self._matches(pid, member) for pid, member in retained):
                raise RuntimeError("VMM process group ownership changed during cleanup")
            self.members.update(added)
            added = {}
        finally:
            for fd, _ in added.values():
                os.close(fd)
        return [
            (pid, member) for pid, member in self.members.items()
            if (stat := self._matches(pid, member)) is not None and stat.alive
        ]

    def terminate(self, *, timeout=5, clock=time.monotonic, sleep=time.sleep):
        for sig in (signal.SIGTERM, signal.SIGKILL):
            deadline = clock() + timeout
            sent = set()
            while live := self._refresh():
                for pid, member in live:
                    fd, _ = member
                    if fd not in sent and self._matches(pid, member) is not None:
                        with contextlib.suppress(ProcessLookupError):
                            signal.pidfd_send_signal(fd, sig)
                        sent.add(fd)
                remaining = deadline - clock()
                if remaining <= 0:
                    break
                sleep(min(CLEANUP_RETRY_INTERVAL, remaining))
            else:
                return
        target = f"process group {self.pgid}" if self.pgid is not None else f"session {self.sid}"
        raise TimeoutError(f"VMM {target} survived cleanup")

    def close(self):
        for fd, _ in self.members.values():
            os.close(fd)
        self.members.clear()


def trial(virtle, fixture, backend, artifacts, teardown):
    artifacts.mkdir()
    result = {
        "backend": backend,
        "shutdown_method": (
            "hard stop through lifecycle RPC"
            if teardown == "kill"
            else (
                "guest Ctrl-Alt-Del/reset"
                if backend == "firecracker"
                else "absent QGA probe, then QMP quit"
            )
        ),
    }
    output = bytearray()
    ready = threading.Event()
    ready_at = None
    overflow = False
    with tempfile.TemporaryDirectory(prefix="virtle-e2e-") as work:
        work = Path(work)
        tmp = work / "tmp"
        tmp.mkdir()
        env = os.environ | {"TMPDIR": str(tmp)}
        command = [virtle, "--manifest", str(fixture / f"{backend}.toml")]
        result["command"] = command + ["launch"]
        start = time.perf_counter()
        owned_vmm = None
        reader = None
        try:
            guest = subprocess.Popen(
                command + ["launch"],
                cwd=work,
                env=env,
                start_new_session=True,
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,
            )
        except OSError as error:
            result.update(passed=False, error=str(error))
            (artifacts / "trial.json").write_text(json.dumps(result, indent=2) + "\n")
            raise

        try:
            owned_vmm = OwnedVMM.capture_launcher(guest.pid)

            def drain():
                nonlocal ready_at, overflow
                try:
                    while data := os.read(guest.stdout.fileno(), 8192):
                        if len(output) + len(data) > OUTPUT_LIMIT:
                            overflow = True
                            ready.set()
                            continue
                        output.extend(data)
                        lines = output.replace(b"\r\n", b"\n").splitlines(keepends=True)
                        if ready_at is None and READY + b"\n" in lines:
                            ready_at = time.perf_counter()
                            ready.set()
                finally:
                    ready.set()

            reader = threading.Thread(target=drain, daemon=True)
            reader.start()
            if not ready.wait(TIMEOUT) or ready_at is None:
                raise RuntimeError("guest exited or timed out before readiness")
            result["process_to_ready_seconds"] = ready_at - start

            def probe_status(timeout):
                status_run = subprocess.run(
                    command + ["status"],
                    cwd=work,
                    env=env,
                    capture_output=True,
                    text=True,
                    timeout=timeout,
                )
                result["status_exit"] = status_run.returncode
                result["status_stderr"] = status_run.stderr
                status_run.check_returncode()
                return json.loads(status_run.stdout)

            # The serial marker can precede the control listener. Keep status
            # retries outside both the readiness and teardown measurements.
            status = wait_for_status(probe_status)
            result["status"] = status
            owned_vmm.validate_status(status["pid"])
            if status["state"] != "ready" or status["pid"] <= 0:
                raise RuntimeError(f"invalid running status: {status}")
            os.kill(status["pid"], 0)
            sockets = [
                Path(status["paths"][key]) for key in ("controlSocket", "qmpSocket")
            ]
            if not all(path.is_socket() for path in sockets):
                raise RuntimeError(
                    "status must identify live control and monitor sockets"
                )
            teardown_start = time.perf_counter()
            teardown_run = subprocess.run(
                command + ["rpc", teardown_rpc(teardown)],
                cwd=work,
                env=env,
                capture_output=True,
                text=True,
                timeout=TIMEOUT,
            )
            result["teardown_rpc_exit"] = teardown_run.returncode
            result["teardown_rpc_stdout"] = teardown_run.stdout
            result["teardown_rpc_stderr"] = teardown_run.stderr
            teardown_run.check_returncode()
            result["launch_exit"] = owned_vmm.wait_launcher(guest, timeout=TIMEOUT)
            result["teardown_seconds"] = time.perf_counter() - teardown_start
            reader.join(timeout=TIMEOUT)
            if reader.is_alive() or overflow:
                raise RuntimeError("console did not close or exceeded 1 MiB")
            if not launch_exit_ok(teardown, result["launch_exit"]):
                raise RuntimeError(
                    f"launch exited with {result['launch_exit']} after {teardown}"
                )
            result["guest_shutdown_marker"] = SHUTDOWN in output.splitlines()
            if (
                teardown == "shutdown"
                and backend == "firecracker"
                and not result["guest_shutdown_marker"]
            ):
                raise RuntimeError("Firecracker guest did not finish shutdown")
            result["sockets_removed"] = all(not path.exists() for path in sockets)
            result["temporary_runtime_removed"] = not any(tmp.iterdir())
            remaining = [str(p) for p in work.rglob("*") if p.is_socket()]
            if (
                not result["sockets_removed"]
                or not result["temporary_runtime_removed"]
                or remaining
            ):
                raise RuntimeError(f"runtime cleanup incomplete: {remaining}")
            try:
                os.kill(status["pid"], 0)
            except ProcessLookupError:
                result["vmm_exited"] = True
            else:
                raise RuntimeError("VMM PID still exists after launch exit")
            result["passed"] = True
        except BaseException as error:
            result["passed"] = False
            result["error"] = str(error)
            print(output.decode(errors="replace"), file=sys.stderr)
            raise
        finally:
            try:
                if owned_vmm is not None and owned_vmm.launcher_alive():
                    # Ask virtle to stop its owned VMM process group first.
                    subprocess.run(
                        command + ["rpc", "kill"],
                        cwd=work,
                        env=env,
                        capture_output=True,
                        timeout=5,
                        check=False,
                    )
            except (OSError, subprocess.TimeoutExpired) as error:
                result.setdefault("cleanup_errors", []).append(str(error))
            finally:
                if owned_vmm is not None:
                    try:
                        if not result.get("passed", False):
                            owned_vmm.terminate()
                            result["failure_vmm_exited"] = True
                    except (OSError, RuntimeError, TimeoutError) as error:
                        result.setdefault("cleanup_errors", []).append(str(error))
                    finally:
                        owned_vmm.close()
                else:
                    guest.kill()
                try:
                    guest.wait(timeout=TIMEOUT)
                except (OSError, subprocess.TimeoutExpired) as error:
                    result.setdefault("cleanup_errors", []).append(str(error))
            if reader is not None and reader.ident is not None:
                reader.join(timeout=TIMEOUT)
            guest.stdout.close()
            (artifacts / "console.log").write_bytes(output)
            (artifacts / "trial.json").write_text(json.dumps(result, indent=2) + "\n")
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--virtle", required=True)
    parser.add_argument("--fixture", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument(
        "--pairs",
        type=int,
        default=10,
        help="measured pairs, positive and even (default: 10)",
    )
    parser.add_argument(
        "--warmup-pairs",
        type=int,
        default=2,
        help="unmeasured pairs, nonnegative and even (default: 2)",
    )
    parser.add_argument(
        "--teardown",
        choices=("kill", "shutdown"),
        default="kill",
        help="lifecycle RPC after readiness; kill keeps fast suites fast (default: kill)",
    )
    args = parser.parse_args()
    if (
        args.pairs < 2
        or args.pairs % 2
        or args.warmup_pairs < 0
        or args.warmup_pairs % 2
    ):
        parser.error(
            "pairs must be positive and even; warmup-pairs nonnegative and even"
        )
    args.output.mkdir(parents=True, exist_ok=False)
    fixture = args.fixture.resolve()
    report = {
        "schema_version": 1,
        "label": LABEL,
        "started_at_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "host": {
            "uname": list(platform.uname()),
            "cpu_affinity": sorted(os.sched_getaffinity(0)),
            "cpu_model": next(
                (
                    line.split(":", 1)[1].strip()
                    for line in Path("/proc/cpuinfo").read_text().splitlines()
                    if line.startswith("model name")
                ),
                "unknown",
            ),
            "load_average": os.getloadavg(),
        },
        "fixture": str(fixture),
        "pairs": args.pairs,
        "warmup_pairs": args.warmup_pairs,
        "trials": [],
        "passed": False,
    }
    print(LABEL, flush=True)
    try:
        require_kvm()
        report["fixture_versions"] = json.loads((fixture / "fixture.json").read_text())
        report["sha256"] = {
            name: hashlib.sha256((fixture / name).read_bytes()).hexdigest()
            for name in (
                "vmlinux",
                "bzImage",
                "initrd",
                "kernel.config",
                "firecracker.toml",
                "qemu.toml",
            )
        }
        report["sha256"]["virtle"] = hashlib.sha256(
            Path(args.virtle).read_bytes()
        ).hexdigest()
        report["virtle_version"] = subprocess.check_output(
            [args.virtle, "--version"], text=True, timeout=TIMEOUT
        ).strip()
        for index, entry in enumerate(schedule(args.pairs, args.warmup_pairs)):
            directory = args.output / f"{index:03d}-{entry['backend']}"
            # Announce each trial before it starts so a stalled run shows
            # which backend and phase it stopped in.
            print(f"--- trial {index:03d}: {entry['backend']} launch", file=sys.stderr, flush=True)
            try:
                row = trial(
                    args.virtle,
                    fixture,
                    entry["backend"],
                    directory,
                    args.teardown,
                )
            finally:
                raw = directory / "trial.json"
                if raw.exists():
                    row = json.loads(raw.read_text()) | entry
                    raw.write_text(json.dumps(row, indent=2) + "\n")
                    report["trials"].append(row)
            print(json.dumps(row), flush=True)
        report["passed"] = True
    finally:
        report["summary"] = summarize(report["trials"])
        (args.output / "results.json").write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps(report["summary"], indent=2), flush=True)


if __name__ == "__main__":
    main()
