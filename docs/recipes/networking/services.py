"""Local HTTPS and DNS services owned by the networking recipe."""

from collections import deque
from contextlib import contextmanager
from dataclasses import dataclass, field
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import ipaddress
from pathlib import Path
import secrets
import socket
import ssl
import subprocess
import threading


@dataclass(frozen=True)
class Services:
    api_port: int
    dns_port: int
    secret: str = field(repr=False)
    ca_path: Path


class _HTTPServer(ThreadingHTTPServer):
    daemon_threads = False

    def __init__(self, address, handler, context):
        self.context = context
        self.clients = set()
        self.clients_lock = threading.Lock()
        super().__init__(address, handler)

    def get_request(self):
        connection, address = super().get_request()
        connection.settimeout(5)
        # Handshake in the request thread: an idle client must not block
        # accepting other clients or stopping the server.
        try:
            connection = self.context.wrap_socket(
                connection, server_side=True, do_handshake_on_connect=False)
        except BaseException:
            connection.close()
            raise
        with self.clients_lock:
            self.clients.add(connection)
        return connection, address

    def process_request_thread(self, request, address):
        try:
            super().process_request_thread(request, address)
        finally:
            with self.clients_lock:
                self.clients.discard(request)

    def handle_error(self, request, address):
        # TLS disconnects need no traceback or request data in recipe logs.
        pass

    def close_clients(self):
        with self.clients_lock:
            for connection in self.clients:
                try:
                    connection.shutdown(socket.SHUT_RDWR)
                except OSError:
                    pass
                connection.close()


@contextmanager
def _dnsmasq(bind_address):
    # Reserve an available port for both transports while selecting it.
    # dnsmasq binds it itself; a competing bind is reported as startup failure.
    with socket.socket() as tcp, socket.socket(type=socket.SOCK_DGRAM) as udp:
        tcp.bind(("127.0.0.1", 0))
        port = tcp.getsockname()[1]
        udp.bind(("127.0.0.1", port))
    command = [
        "dnsmasq", "--no-daemon", "--conf-file=", "--no-hosts",
        "--bind-interfaces", "--listen-address=127.0.0.1", f"--port={port}",
        "--local=/virtle.test/", "--resolv-file=/etc/resolv.conf",
        f"--host-record=api.virtle.test,other.virtle.test,blocked.virtle.test,{bind_address}",
        "--txt-record=api.virtle.test,virtle networking demo",
    ]
    process = subprocess.Popen(command, stdin=subprocess.DEVNULL,
                               stdout=subprocess.DEVNULL, stderr=subprocess.PIPE,
                               text=True, encoding="utf-8", errors="replace")
    messages = deque(maxlen=32)
    startup = threading.Event()
    started = False

    def drain():
        nonlocal started
        try:
            for line in process.stderr:
                messages.append(line.rstrip())
                if "started, version" in line:
                    started = True
                    startup.set()
        finally:
            startup.set()

    reader = threading.Thread(target=drain, name="recipe-dnsmasq", daemon=True)
    reader.start()
    try:
        if not startup.wait(10) or not started or process.poll() is not None:
            raise RuntimeError("dnsmasq did not start: " + "\n".join(messages))
        yield port
        if process.poll() is not None:
            raise RuntimeError("dnsmasq exited unexpectedly: " + "\n".join(messages))
    finally:
        if process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)
        reader.join(timeout=5)
        process.stderr.close()


@contextmanager
def services(work_dir: Path, bind_address: str):
    """Run the mock API and DNS resolver until the context exits.

    The caller supplies a host IPv4 address reachable through Virtle's egress
    policy and passes ca_path as SSL_CERT_FILE only to the child Virtle process.
    The dummy API credential is generated here and never written to disk.
    """
    address = ipaddress.IPv4Address(bind_address)
    if address.is_loopback or address.is_unspecified or address.is_multicast:
        raise ValueError("the mock API needs a non-loopback host IPv4 address")
    work_dir.mkdir(parents=True, exist_ok=True)
    ca_path = work_dir / "upstream-ca.pem"
    key_path = work_dir / "upstream-key.pem"
    subprocess.run([
        "openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes",
        "-days", "1", "-subj", "/CN=api.virtle.test",
        "-addext", "subjectAltName=DNS:api.virtle.test,DNS:other.virtle.test",
        "-addext", "basicConstraints=critical,CA:TRUE",
        "-keyout", str(key_path), "-out", str(ca_path),
    ], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, timeout=15)
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.load_cert_chain(ca_path, key_path)
    key_path.unlink()  # The loaded TLS context owns the private key now.
    secret = secrets.token_urlsafe(32)

    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            if self.path != "/auth":
                status = 404
            else:
                status = 204 if self.headers.get("Authorization") == "Bearer " + secret else 401
            # Log only known labels; never reflect headers or arbitrary URLs.
            host = self.headers.get("Host", "").lower().partition(":")[0]
            if host not in ("api.virtle.test", "other.virtle.test"):
                host = "unknown"
            path = "/auth" if self.path == "/auth" else "other"
            print(f"mock API host={host} path={path} status={status}", flush=True)
            self.send_error(status)

        def send_error(self, code, message=None, explain=None):
            self.send_response_only(code)
            self.send_header("Content-Length", "0")
            self.send_header("Connection", "close")
            self.end_headers()
            self.close_connection = True

        def log_message(self, format, *args):
            pass

    server = _HTTPServer((str(address), 0), Handler, context)
    worker = threading.Thread(target=server.serve_forever,
                              name="recipe-https", daemon=True)
    worker.start()
    try:
        with _dnsmasq(str(address)) as dns_port:
            yield Services(server.server_port, dns_port, secret, ca_path)
    finally:
        server.shutdown()
        server.close_clients()
        server.server_close()
        worker.join(timeout=5)
