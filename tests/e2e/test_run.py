"""Pure runner checks; real guest behavior is covered by the KVM flake check."""

import contextlib
import json
import os
from pathlib import Path
import select
import signal
import subprocess
import sys
import unittest
from unittest import mock

import run


class FakeClock:
    def __init__(self):
        self.now = 0
        self.sleeps = []

    def __call__(self):
        return self.now

    def sleep(self, seconds):
        self.sleeps.append(seconds)
        self.now += seconds


class StatusReadinessTest(unittest.TestCase):
    def test_status_retries_until_control_socket_is_available(self):
        clock = FakeClock()
        timeouts = []
        ready_status = {"state": "ready", "pid": 42}

        def probe(timeout):
            timeouts.append(timeout)
            if len(timeouts) < 3:
                raise subprocess.CalledProcessError(
                    1, ["virtle", "status"], stderr="control socket not found"
                )
            return ready_status

        status = run.wait_for_status(probe, timeout=1, clock=clock, sleep=clock.sleep)
        self.assertIs(status, ready_status)
        self.assertEqual(len(timeouts), 3)
        self.assertEqual(len(clock.sleeps), 2)
        self.assertEqual(timeouts[0], 1)
        self.assertGreater(timeouts[0], timeouts[1])
        self.assertGreater(timeouts[1], timeouts[2])
        self.assertLess(clock.now, 1)

    def test_status_deadline_includes_command_duration_and_retry_delays(self):
        clock = FakeClock()
        timeouts = []
        unavailable = subprocess.CalledProcessError(
            1, ["virtle", "status"], stderr="connection refused"
        )

        def probe(timeout):
            timeouts.append(timeout)
            clock.now += min(0.2, timeout)
            raise unavailable

        with self.assertRaisesRegex(TimeoutError, "control status") as caught:
            run.wait_for_status(probe, timeout=0.75, clock=clock, sleep=clock.sleep)
        self.assertIs(caught.exception.__cause__, unavailable)
        self.assertAlmostEqual(clock.now, 0.75)
        self.assertGreater(len(timeouts), 1)
        self.assertTrue(all(0 < timeout <= 0.75 for timeout in timeouts))

    def test_status_command_timeout_uses_remaining_deadline(self):
        clock = FakeClock()
        timeouts = []
        expired = subprocess.TimeoutExpired(["virtle", "status"], 0.25)

        def probe(timeout):
            timeouts.append(timeout)
            clock.now += timeout
            raise expired

        with self.assertRaises(TimeoutError) as caught:
            run.wait_for_status(probe, timeout=0.25, clock=clock, sleep=clock.sleep)
        self.assertIs(caught.exception.__cause__, expired)
        self.assertEqual(timeouts, [0.25])
        self.assertEqual(clock.sleeps, [])

    def test_status_success_returns_immediately(self):
        clock = FakeClock()
        status = {"state": "ready", "pid": 42}
        self.assertIs(
            run.wait_for_status(lambda timeout: status, clock=clock, sleep=clock.sleep),
            status,
        )
        self.assertEqual(clock.sleeps, [])


class ComparisonTest(unittest.TestCase):
    def test_counterbalanced_pairs_and_separate_warmups(self):
        trials = list(run.schedule(pairs=4, warmup_pairs=2))
        self.assertEqual(
            [t["backend"] for t in trials[:4]],
            ["firecracker", "qemu", "qemu", "firecracker"],
        )
        self.assertTrue(all(t["warmup"] for t in trials[:4]))
        measured = trials[4:]
        self.assertTrue(all(not t["warmup"] for t in measured))
        for backend in ("firecracker", "qemu"):
            self.assertEqual(sum(t["backend"] == backend for t in measured), 4)
            self.assertEqual(
                sum(t["backend"] == backend and t["position"] == 0 for t in measured), 2
            )

    def test_teardown_rpc_is_explicit_and_fast_by_default(self):
        self.assertEqual(run.teardown_rpc("kill"), "kill")
        self.assertEqual(run.teardown_rpc("shutdown"), "shutdown")
        with self.assertRaises(ValueError):
            run.teardown_rpc("unknown")

    def test_hard_stop_accepts_only_the_expected_launch_exit(self):
        self.assertTrue(run.launch_exit_ok("kill", 1))
        self.assertFalse(run.launch_exit_ok("kill", 0))
        self.assertTrue(run.launch_exit_ok("shutdown", 0))
        self.assertFalse(run.launch_exit_ok("shutdown", 1))

    def test_summary_uses_only_successful_measured_trials(self):
        rows = [
            {
                "backend": "firecracker",
                "warmup": warmup,
                "passed": passed,
                "process_to_ready_seconds": ready,
                "teardown_seconds": teardown,
            }
            for warmup, passed, ready, teardown in [
                (True, True, 100, 200),
                (False, True, 1, 4),
                (False, True, 3, 6),
                (False, False, 99, 99),
            ]
        ]
        summary = run.summarize(rows)["firecracker"]
        self.assertEqual(summary["trials"], 2)
        self.assertEqual(
            summary["process_to_ready_seconds"], {"min": 1, "median": 2, "max": 3}
        )
        self.assertEqual(summary["teardown_seconds"], {"min": 4, "median": 5, "max": 6})


class TrialCleanupTest(unittest.TestCase):
    @contextlib.contextmanager
    def runner(self, *, successful=False, launcher_live=False, state="ready"):
        guest = mock.Mock(pid=101, returncode=1)
        guest.poll.return_value = None if launcher_live else 1
        guest.wait.return_value = 1
        artifacts = mock.MagicMock()
        work = mock.MagicMock()
        work.__truediv__.return_value = work
        work.is_socket.return_value = successful
        work.exists.return_value = False
        work.iterdir.return_value = iter(())
        work.rglob.return_value = iter(())
        status = {
            "state": state,
            "pid": 202,
            "paths": {"controlSocket": "control", "qmpSocket": "monitor"},
        }

        def thread(*, target, **kwargs):
            reader = mock.Mock()
            reader.start.side_effect = target
            reader.is_alive.return_value = False
            return reader

        with contextlib.ExitStack() as stack:
            patches = {
                "Path": mock.Mock(return_value=work),
                "tempfile.TemporaryDirectory": mock.Mock(
                    return_value=contextlib.nullcontext("work")
                ),
                "subprocess.Popen": mock.Mock(return_value=guest),
                "subprocess.run": mock.Mock(return_value=mock.Mock(
                    returncode=0, stdout="", stderr=""
                )),
                "threading.Thread": thread,
                "os.read": mock.Mock(side_effect=[run.READY + b"\n", b""]),
                "os.kill": mock.Mock(side_effect=[None, ProcessLookupError()]),
                "os.killpg": mock.Mock(),
                "wait_for_status": mock.Mock(return_value=status),
            }
            for name, value in patches.items():
                stack.enter_context(mock.patch("run." + name, value))
            # Mock the ownership boundary while exercising the real trial/finally.
            ownership = stack.enter_context(mock.patch.object(run, "OwnedVMM", create=True))
            owner = ownership.capture_launcher.return_value
            owner.launcher_alive.return_value = launcher_live
            owner.wait_launcher.return_value = 1
            yield guest, ownership, artifacts, patches["subprocess.run"]

    def test_exited_launcher_still_cleans_separate_vmm_group(self):
        with self.runner() as (guest, ownership, artifacts, command):
            with self.assertRaisesRegex(RuntimeError, "live control and monitor"):
                run.trial("virtle", Path("fixture"), "qemu", artifacts, "kill")
            ownership.capture_launcher.assert_called_once_with(guest.pid)
            ownership.capture_launcher.return_value.validate_status.assert_called_once_with(202)
            ownership.capture_launcher.return_value.terminate.assert_called_once_with()
            ownership.capture_launcher.return_value.close.assert_called_once_with()
            command.assert_not_called()

    def test_ownership_is_retained_before_status_validation(self):
        with self.runner(state="stopping") as (guest, ownership, artifacts, _):
            with self.assertRaisesRegex(RuntimeError, "invalid running status"):
                run.trial("virtle", Path("fixture"), "qemu", artifacts, "kill")
            ownership.capture_launcher.assert_called_once_with(guest.pid)
            ownership.capture_launcher.return_value.validate_status.assert_called_once_with(202)
            ownership.capture_launcher.return_value.terminate.assert_called_once_with()

    def test_ownership_precedes_console_thread_creation(self):
        with self.runner() as (guest, ownership, _, _):
            with mock.patch("run.threading.Thread", side_effect=RuntimeError("reader unavailable")):
                with self.assertRaisesRegex(RuntimeError, "reader unavailable"):
                    run.trial("virtle", Path("fixture"), "qemu", mock.MagicMock(), "kill")
            ownership.capture_launcher.assert_called_once_with(guest.pid)
            ownership.capture_launcher.return_value.terminate.assert_called_once_with()
            ownership.capture_launcher.return_value.close.assert_called_once_with()
            guest.wait.assert_called_once()

    def test_launcher_cleanup_error_still_cleans_vmm_and_writes_diagnostics(self):
        with self.runner(launcher_live=True) as (_, ownership, artifacts, command):
            command.side_effect = OSError("kill RPC unavailable")
            with self.assertRaisesRegex(RuntimeError, "live control and monitor"):
                run.trial("virtle", Path("fixture"), "qemu", artifacts, "kill")
            ownership.capture_launcher.return_value.terminate.assert_called_once_with()
            report = json.loads(artifacts.__truediv__.return_value.write_text.call_args.args[0])
            self.assertIn("kill RPC unavailable", str(report["cleanup_errors"]))

    def test_success_keeps_normal_cleanup(self):
        with self.runner(successful=True) as (guest, ownership, artifacts, command):
            result = run.trial("virtle", Path("fixture"), "qemu", artifacts, "kill")
            self.assertTrue(result["passed"])
            ownership.capture_launcher.return_value.terminate.assert_not_called()
            ownership.capture_launcher.return_value.close.assert_called_once_with()
            guest.wait.assert_called_once()
            self.assertEqual(command.call_count, 1)


class FakeProcesses:
    def __init__(self):
        self.processes = {}
        self.handles = {}
        self.signals = []
        self.ignore = set()
        self.next_fd = 1000

    def add(self, pid, *, pgid=202, sid=101, start=1, state="S", threads=1):
        self.processes[pid] = (pgid, sid, start, state, threads)

    def path(self, name):
        path = Path(name)
        entry = mock.Mock(name=path.name)
        entry.name = path.name
        if str(path) == "/proc":
            entry.iterdir.side_effect = lambda: [self.path(f"/proc/{pid}") for pid in self.processes]
        else:
            def read():
                pid = int(path.parent.name)
                if pid not in self.processes:
                    raise FileNotFoundError()
                pgid, sid, start, state, threads = self.processes[pid]
                fields = [state, "101", str(pgid), str(sid)] + ["0"] * 13
                fields += [str(threads), "0", str(start)]
                return f"{pid} (fake (vmm)) " + " ".join(fields)
            entry.read_text.side_effect = read
        return entry

    def open(self, pid):
        if pid not in self.processes:
            raise ProcessLookupError()
        fd = self.next_fd
        self.next_fd += 1
        self.handles[fd] = (pid, self.processes[pid][2])
        return fd

    def send(self, fd, sig):
        pid, start = self.handles[fd]
        if pid not in self.processes or self.processes[pid][2] != start:
            raise ProcessLookupError()
        if sig:
            self.signals.append((pid, sig))
            if sig not in self.ignore:
                del self.processes[pid]

    @contextlib.contextmanager
    def installed(self):
        with mock.patch("run.Path", self.path), mock.patch("run.os.pidfd_open", self.open), mock.patch("run.signal.pidfd_send_signal", self.send), mock.patch("run.os.close", side_effect=lambda fd: self.handles.pop(fd)), mock.patch("run.os.killpg") as killpg:
            yield
            killpg.assert_not_called()


class OwnedVMMTest(unittest.TestCase):
    def setUp(self):
        self.proc = FakeProcesses()
        self.proc.add(202)
        self.clock = FakeClock()
        self.addCleanup(self.assertEqual, self.proc.handles, {})
        self.enterContext(self.proc.installed())

    def capture(self):
        owner = run.OwnedVMM.capture(202, 101)
        self.addCleanup(owner.close)
        return owner

    def terminate(self, owner):
        owner.terminate(timeout=1, clock=self.clock, sleep=self.clock.sleep)

    def test_terminates_all_members_of_separate_group(self):
        self.proc.add(203)
        self.proc.add(999, pgid=999, sid=999)
        self.terminate(self.capture())
        self.assertEqual(set(self.proc.signals), {(202, signal.SIGTERM), (203, signal.SIGTERM)})
        self.assertEqual(set(self.proc.processes), {999})

    def test_retains_descendant_ownership_after_group_leader_exits(self):
        self.proc.add(203)
        owner = self.capture()
        del self.proc.processes[202]
        self.terminate(owner)
        self.assertEqual(self.proc.signals, [(203, signal.SIGTERM)])

    def test_discovers_members_started_after_status(self):
        owner = self.capture()
        self.proc.add(203)
        self.terminate(owner)
        self.assertEqual(set(self.proc.signals), {(202, signal.SIGTERM), (203, signal.SIGTERM)})

    def test_escalates_and_verifies_exit_with_bounded_waits(self):
        self.proc.ignore.add(signal.SIGTERM)
        self.terminate(self.capture())
        self.assertEqual(self.proc.signals, [(202, signal.SIGTERM), (202, signal.SIGKILL)])
        self.assertLessEqual(self.clock.now, 2)
        self.assertEqual(self.proc.processes, {})

    def test_reports_survivor_at_deadline(self):
        self.proc.ignore.update((signal.SIGTERM, signal.SIGKILL))
        with self.assertRaisesRegex(TimeoutError, "VMM process group"):
            self.terminate(self.capture())
        self.assertEqual(self.clock.now, 2)

    def test_reused_pid_and_pgid_are_safe(self):
        owner = self.capture()
        self.proc.add(202, start=2)
        self.proc.add(204, start=2)
        self.terminate(owner)
        self.assertEqual(self.proc.signals, [])

    def test_rejects_groups_outside_launcher_session(self):
        self.proc.add(202, sid=999)
        with self.assertRaisesRegex(RuntimeError, "owned"):
            self.capture()
        self.assertEqual(self.proc.signals, [])

    def test_rejects_a_status_pid_that_is_not_a_group_leader(self):
        self.proc.add(202, pgid=101)
        with self.assertRaisesRegex(RuntimeError, "owned"):
            self.capture()

    def test_stopped_zombie_is_complete(self):
        owner = self.capture()
        self.proc.add(202, state="Z")
        self.terminate(owner)
        self.assertEqual(self.proc.signals, [])


class OwnedSessionTest(unittest.TestCase):
    def setUp(self):
        self.proc = FakeProcesses()
        self.proc.add(101, pgid=101)
        self.clock = FakeClock()
        self.addCleanup(self.assertEqual, self.proc.handles, {})
        self.enterContext(self.proc.installed())

    def capture(self):
        owner = run.OwnedVMM.capture_launcher(101)
        self.addCleanup(owner.close)
        return owner

    def terminate(self, owner):
        owner.terminate(timeout=1, clock=self.clock, sleep=self.clock.sleep)

    def test_discovers_separate_groups_after_launcher_exit_without_status(self):
        owner = self.capture()
        self.proc.add(101, pgid=101, state="Z")
        self.proc.add(202)
        self.proc.add(203)
        self.proc.add(204, pgid=204)
        self.proc.add(999, pgid=999, sid=999)
        self.assertFalse(owner.launcher_alive())
        self.terminate(owner)
        self.assertEqual(set(self.proc.signals), {
            (202, signal.SIGTERM), (203, signal.SIGTERM), (204, signal.SIGTERM),
        })
        self.assertEqual(set(self.proc.processes), {101, 999})

    def test_reused_launcher_pid_and_session_are_safe(self):
        owner = self.capture()
        self.proc.add(101, pgid=101, start=2)
        self.proc.add(202, start=2)
        self.terminate(owner)
        self.assertEqual(self.proc.signals, [])

    def test_rechecks_session_anchor_after_opening_new_members(self):
        owner = self.capture()
        self.proc.add(202)
        original_open = self.proc.open

        def replace_session(pid):
            self.proc.add(101, pgid=101, start=2)
            self.proc.add(202, start=2)
            return original_open(pid)

        with mock.patch("run.os.pidfd_open", side_effect=replace_session):
            with self.assertRaisesRegex(RuntimeError, "ownership changed"):
                self.terminate(owner)
        self.assertEqual(self.proc.signals, [])

    def test_validates_status_against_pinned_session_members(self):
        owner = self.capture()
        self.proc.add(202)
        self.proc.add(203)
        self.proc.add(999, pgid=999, sid=999)
        owner.validate_status(202)
        for pid in (101, 203, 999, 0, "202"):
            with self.subTest(pid=pid):
                with self.assertRaisesRegex(RuntimeError, "owned"):
                    owner.validate_status(pid)

    def test_session_cleanup_escalates_with_bounded_waits(self):
        owner = self.capture()
        self.proc.add(202)
        self.proc.ignore.add(signal.SIGTERM)
        self.terminate(owner)
        self.assertEqual(set(self.proc.signals), {
            (pid, sig) for pid in (101, 202) for sig in (signal.SIGTERM, signal.SIGKILL)
        })
        self.assertLessEqual(self.clock.now, 2)


@unittest.skipUnless(sys.platform == "linux", "Linux process groups and pidfds")
class LiveProcessCleanupTest(unittest.TestCase):
    def test_trial_cleans_vmm_when_launcher_exits_before_readiness_or_status(self):
        # Keep the exited launcher unreaped, just as Popen does. The child
        # handshakes before launcher exit and never supplies a status PID.
        for marker in (b"", run.READY + b"\n"):
            with self.subTest(readiness=bool(marker)):
                child_code = "import signal; print('ready', flush=True); signal.pause()"
                launcher_code = (
                    "import subprocess, sys\n"
                    f"child = subprocess.Popen([sys.executable, '-c', {child_code!r}], "
                    "process_group=0, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)\n"
                    "assert child.stdout.readline() == b'ready\\n'\n"
                    "print(child.pid, flush=True)\n"
                    f"sys.stdout.buffer.write({marker!r})\n"
                )
                popen = subprocess.Popen
                launcher = None
                child_fd = None

                def spawn(*args, **kwargs):
                    nonlocal launcher, child_fd
                    launcher = popen([sys.executable, "-c", launcher_code], bufsize=0, **kwargs)
                    child_pid = int(launcher.stdout.readline())
                    child_fd = os.pidfd_open(child_pid)
                    self.assertEqual(os.getpgid(child_pid), child_pid)
                    self.assertEqual(os.getsid(child_pid), launcher.pid)
                    with contextlib.ExitStack() as stack:
                        launcher_fd = os.pidfd_open(launcher.pid)
                        stack.callback(os.close, launcher_fd)
                        self.assertTrue(select.select([launcher_fd], [], [], run.TIMEOUT)[0])
                    os.waitid(os.P_PID, launcher.pid, os.WEXITED | os.WNOWAIT)
                    return launcher

                artifacts = mock.MagicMock()
                try:
                    with mock.patch("run.subprocess.Popen", side_effect=spawn), mock.patch(
                        "run.wait_for_status", side_effect=TimeoutError("status unavailable")
                    ) as status:
                        expected = "status unavailable" if marker else "before readiness"
                        with self.assertRaisesRegex((RuntimeError, TimeoutError), expected):
                            run.trial("virtle", Path("fixture"), "qemu", artifacts, "kill")
                    self.assertEqual(status.call_count, int(bool(marker)))
                    self.assertTrue(
                        select.select([child_fd], [], [], 0)[0],
                        "trial cleanup must stop the surviving VMM without a status PID",
                    )
                    report = json.loads(artifacts.__truediv__.return_value.write_text.call_args.args[0])
                    self.assertTrue(report["failure_vmm_exited"])
                    self.assertNotIn("cleanup_errors", report)
                finally:
                    if child_fd is not None:
                        with contextlib.suppress(ProcessLookupError):
                            signal.pidfd_send_signal(child_fd, signal.SIGKILL)
                        os.close(child_fd)
                    if launcher is not None:
                        launcher.wait(timeout=run.TIMEOUT)
                        launcher.stdout.close()

    def test_exited_launcher_with_surviving_separate_process_group(self):
        # Pipe handshakes fix the ordering; no guest, files, or timed sleeps.
        child = "import signal; print('ready', flush=True); signal.pause()"
        launcher_code = (
            "import subprocess, sys\n"
            f"child = subprocess.Popen([sys.executable, '-c', {child!r}], process_group=0, stdout=subprocess.PIPE)\n"
            "assert child.stdout.readline() == b'ready\\n'\n"
            "print(child.pid, flush=True)\n"
            "sys.stdin.buffer.read(1)\n"
        )
        launcher = subprocess.Popen(
            [sys.executable, "-c", launcher_code], start_new_session=True,
            stdin=subprocess.PIPE, stdout=subprocess.PIPE,
        )
        owner = None
        try:
            pid = int(launcher.stdout.readline())
            owner = run.OwnedVMM.capture(pid, launcher.pid)
            self.assertEqual(os.getpgid(pid), pid)
            self.assertNotEqual(pid, launcher.pid)
            launcher.communicate(b"x", timeout=run.TIMEOUT)
            self.assertIsNotNone(launcher.poll())
            os.kill(pid, 0)
            owner.terminate(timeout=1)
            stat = run.process_stat(Path(f"/proc/{pid}/stat").read_text()) if Path(f"/proc/{pid}/stat").exists() else None
            self.assertTrue(stat is None or not stat.alive)
        finally:
            if owner is not None:
                owner.terminate(timeout=1)
                owner.close()
            if launcher.poll() is None:
                launcher.kill()
            launcher.communicate(timeout=run.TIMEOUT)


if __name__ == "__main__":
    unittest.main()
