"""Launch the QEMU networking playground and its disposable host services."""

import argparse
import ipaddress
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import threading

from services import services


def host_address(requested):
    addresses = json.loads(subprocess.check_output(
        ["ip", "-j", "-4", "address", "show", "scope", "global"], timeout=5))
    candidates = [entry["local"] for interface in addresses
                  for entry in interface.get("addr_info", [])]
    denied = tuple(ipaddress.IPv4Network(prefix) for prefix in (
        "0.0.0.0/8", "127.0.0.0/8", "169.254.0.0/16", "224.0.0.0/4",
        "255.255.255.255/32"))

    def permitted(address):
        return not any(ipaddress.IPv4Address(address) in prefix for prefix in denied)

    if requested:
        requested = str(ipaddress.IPv4Address(requested))
        if requested not in candidates:
            raise ValueError("--bind-address must be an IPv4 address assigned to a host interface")
        if not permitted(requested):
            raise ValueError("--bind-address is denied by the default egress policy")
        return requested
    candidates = [address for address in candidates if permitted(address)]
    if not candidates:
        raise ValueError("the demo needs an interface IPv4 address permitted by egress; loopback and link-local addresses are denied")
    return candidates[0]


def stop_guest(guest, virtle, work_dir):
    if guest.poll() is not None:
        return
    # Give virtle its usual shutdown path before escalating. RPC failure must
    # not skip the final process kill and reap.
    try:
        guest.terminate()
        guest.wait(timeout=25)
    except subprocess.TimeoutExpired:
        try:
            subprocess.run([virtle, "rpc", "kill"], cwd=work_dir,
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                           timeout=5, check=False)
        except (OSError, subprocess.SubprocessError) as error:
            print(f"Control-socket shutdown failed: {error}", file=sys.stderr)
    finally:
        if guest.poll() is None:
            guest.kill()
        guest.wait(timeout=5)


def require_log(output, event, **fields):
    # The CLI's default slog handler writes "INFO <message> key=value ...".
    # These demo fields contain no spaces, so exact tokens also distinguish
    # api.virtle.test from another hostname sharing its suffix.
    for line in output.splitlines():
        if f"INFO {event} " in line and all(
                f"{key}={value}" in line.split() for key, value in fields.items()):
            return line
    raise RuntimeError(f"missing {event} result: {fields}")


def launch(args, work_dir, demo):
    template = Path(__file__).with_name("manifest.toml").read_text()
    for name, value in {"DNS_PORT": demo.dns_port, "API_PORT": demo.api_port,
                        "HOST_ADDRESS": args.bind_address}.items():
        template = template.replace(f"@{name}@", str(value))
    (work_dir / "manifest.toml").write_text(args.base_manifest.read_text() + "\n" + template)
    subprocess.run(["qemu-img", "create", "-q", "-f", "qcow2", "-F", "qcow2",
                    "-b", str(args.disk), str(work_dir / "root.qcow2")], check=True, timeout=30)

    # The host-only dummy credential is generated for each run. The guest gets
    # its placeholder through the manifest loader. Trust this run's mock API
    # certificate only in the virtle process doing the upstream TLS connection.
    env = os.environ | {"VIRTLE_DEMO_SECRET": demo.secret, "SSL_CERT_FILE": str(demo.ca_path)}
    command = [args.virtle, "-v", "launch", "--resume=no", "--ssh"]
    if args.check:
        command += ["demo-check"]
    print(f"Workspace: {work_dir}\nDNS/egress log: {work_dir / 'network.log'}", flush=True)
    if not args.check:
        print("In the guest, run demo-check. Exit the shell to stop the VM.\n"
              "On the host, tail -f the log above to watch DNS and HTTPS decisions.", flush=True)

    with (work_dir / "network.log").open("wb") as log:
        guest = subprocess.Popen(command, cwd=work_dir, env=env,
                                 stdin=subprocess.DEVNULL if args.check else None,
                                 stdout=subprocess.PIPE if args.check else None,
                                 stderr=subprocess.STDOUT if args.check else subprocess.PIPE)
        stream = guest.stdout if args.check else guest.stderr

        def drain():
            with stream:
                for data in stream:
                    log.write(data)
                    log.flush()
                    if args.check and data.startswith((b"PASS:", b"FAIL:")):
                        sys.stdout.buffer.write(data)
                        sys.stdout.buffer.flush()

        reader = threading.Thread(target=drain, daemon=True)
        reader.start()
        try:
            result = guest.wait(timeout=180 if args.check else None)
        finally:
            try:
                stop_guest(guest, args.virtle, work_dir)
            finally:
                reader.join(timeout=5)
        if reader.is_alive():
            raise RuntimeError("virtle output did not close")
    if result != 0:
        raise RuntimeError(f"virtle exited with status {result}; see {work_dir / 'network.log'}")
    if args.check:
        output = (work_dir / "network.log").read_text()
        if "PASS: networking demo complete" not in output:
            raise RuntimeError("guest did not finish the demo checks")
        for record_type in ("A", "TXT"):
            require_log(output, "dns query", name="api.virtle.test", type=record_type,
                        decision="allow", rcode="NOERROR")
        for name in ("blocked.virtle.test", "unlisted.test"):
            require_log(output, "dns query", name=name, type="A", decision="deny", rcode="REFUSED")
        require_log(output, "egress flow", host="api.virtle.test", method="GET",
                    path="/auth", status=401, decision="allow")
        require_log(output, "egress flow", host="api.virtle.test", method="GET",
                    path="/auth", status=204, decision="allow", injections="[DEMO_TOKEN]")
        other = require_log(output, "egress flow", host="other.virtle.test", method="GET",
                            path="/auth", status=401, decision="allow")
        if "injections=" in other:
            raise RuntimeError("another hostname received a secret injection")
        require_log(output, "egress flow", decision="deny", proto="tcp",
                    dst=f"{args.bind_address}:{demo.api_port}")
        if demo.secret in output:
            raise RuntimeError("dummy credential appeared in the guest output or network log")
        print("PASS: DNS/egress decisions recorded; host credential absent from output")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--virtle", required=True)
    parser.add_argument("--base-manifest", type=Path, required=True)
    parser.add_argument("--disk", type=Path, required=True)
    parser.add_argument("--check", action="store_true", help="run the guest checks and exit")
    parser.add_argument("--bind-address", help="host IPv4 for the mock API (default: first interface address)")
    parser.add_argument("--keep", action="store_true", help="retain the disposable workspace and logs")
    args = parser.parse_args()
    args.bind_address = host_address(args.bind_address)
    work_dir = Path(tempfile.mkdtemp(prefix="virtle-net-"))
    keep = args.keep
    try:
        with services(work_dir, args.bind_address) as demo:
            launch(args, work_dir, demo)
    except BaseException:
        keep = True
        raise
    finally:
        if keep:
            print(f"Kept workspace: {work_dir}", file=sys.stderr)
        else:
            shutil.rmtree(work_dir)


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, RuntimeError, subprocess.SubprocessError) as error:
        sys.exit(str(error))
    except KeyboardInterrupt:
        print("Interrupted; stopping the demo.", file=sys.stderr)
        sys.exit(130)
