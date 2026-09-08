"""Boot through the public CLI, require guest work, and verify graceful exit."""

import json
import os
from pathlib import Path
import subprocess
import sys
import threading


def check(virtle, manifest):
    command = [virtle, "--manifest", manifest]
    guest = subprocess.Popen(command + ["launch"], stdout=subprocess.PIPE,
                             stderr=subprocess.STDOUT)
    output = bytearray()
    ready = threading.Event()
    too_large = threading.Event()

    def drain():
        try:
            while data := os.read(guest.stdout.fileno(), 8192):
                if len(output) + len(data) <= 1024 * 1024:
                    output.extend(data)
                else:
                    too_large.set()
                if b"VIRTLE_READY:42" in output or too_large.is_set():
                    ready.set()
        finally:
            ready.set()

    # Keep draining while synchronous status/shutdown RPCs run, so pipe
    # backpressure cannot prevent the guest or VMM from completing shutdown.
    reader = threading.Thread(target=drain, daemon=True)
    reader.start()
    try:
        if not ready.wait(60):
            raise RuntimeError("guest operation did not become ready within 60s")
        if b"VIRTLE_READY:42" not in output:
            raise RuntimeError("virtle exited before guest readiness")
        status = json.loads(subprocess.check_output(command + ["status"], timeout=10))
        socket = status["paths"]["qmpSocket"]
        if not Path(socket).is_socket():
            raise RuntimeError("status did not identify a live API socket")
        subprocess.run(command + ["rpc", "shutdown"], check=True, timeout=20)
        guest.wait(timeout=10)
        reader.join(timeout=10)
        if reader.is_alive() or too_large.is_set():
            raise RuntimeError("console did not close or exceeded 1 MiB")
        if guest.returncode != 0:
            raise RuntimeError(f"virtle exited with {guest.returncode}")
        if b"VIRTLE_SHUTDOWN:unmounted" not in output:
            raise RuntimeError("guest did not cleanly unmount its disk")
        if Path(socket).parent.exists() or Path(status["paths"]["controlSocket"]).exists():
            raise RuntimeError("runtime sockets were not cleaned up")
        print("PASS: guest read raw disk, computed 42, unmounted, stopped; sockets removed")
    finally:
        if guest.poll() is None:
            try:
                subprocess.run(command + ["rpc", "kill"], timeout=5, check=False)
            finally:
                guest.terminate()
                try:
                    guest.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    guest.kill()
                    guest.wait()
        reader.join(timeout=10)
        sys.stdout.buffer.write(output)


if __name__ == "__main__":
    check(*sys.argv[1:])
