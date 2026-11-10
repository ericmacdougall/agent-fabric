"""Synthetic-only controls for the private exec-only libpq reference adapter.

The actual fleet psql authenticates to an in-process wire-protocol double; this
module never starts PostgreSQL, executes database SQL, or reads real credentials.
Run the positive control against the initial adapter stub before implementation.
"""
from __future__ import annotations

import contextlib
import fcntl
import hashlib
import os
from pathlib import Path
import socket
import stat
import struct
import subprocess
import sys
import tempfile
import threading
import time
import unittest
import urllib.parse

ROOT = Path(__file__).resolve().parent
ADAPTER = ROOT / "pgcall_ref.py"
PYTHON = "/usr/bin/python3"
CLIENT = Path("/usr/lib/postgresql/16/bin/psql")
CLIENT_SHA256 = "df8031178589c31c7bc20aa2adf902e1cc3f0aa9ea12428aa5db3d476cbcac1a"
CLEAN_ENV = {"PATH": "/usr/bin:/bin", "LANG": "C.UTF-8"}
SYNTHETIC_PASSWORD = "fixture-only:reference\\control-731"


def _exact(conn: socket.socket, count: int) -> bytes:
    parts = bytearray()
    while len(parts) < count:
        block = conn.recv(count - len(parts))
        if not block:
            raise EOFError
        parts.extend(block)
    return bytes(parts)


def _packet(kind: bytes, body: bytes) -> bytes:
    return kind + struct.pack("!I", len(body) + 4) + body


class FakePostgres:
    """One trusted synthetic credential exchange and one known row response."""

    def __init__(self, password: str = SYNTHETIC_PASSWORD, pause: bool = False, auth_mode: str = "password"):
        if auth_mode not in ("password", "none"):
            raise ValueError("unknown synthetic auth control mode")
        self.password = password.encode()
        self.pause = pause
        self.auth_mode = auth_mode
        self.connected = threading.Event()
        self.auth_received = threading.Event()
        self.no_auth_offered = threading.Event()
        self.release = threading.Event()
        self.closed = threading.Event()
        self.authenticated = False
        self.query_seen = False
        self.startup_user = None
        self.startup_database = None
        self.failed = False
        self.connection = None
        self.listener = socket.socket()
        self.listener.bind(("127.0.0.1", 0))
        self.listener.listen(1)
        self.listener.settimeout(0.1)
        self.port = self.listener.getsockname()[1]
        self.thread = threading.Thread(target=self._serve, daemon=True)

    def __enter__(self):
        self.thread.start()
        return self

    def __exit__(self, *_):
        self.closed.set()
        self.release.set()
        if self.connection is not None:
            with contextlib.suppress(OSError):
                self.connection.shutdown(socket.SHUT_RDWR)
        self.listener.close()
        self.thread.join(timeout=1)

    def _serve(self):
        try:
            deadline = time.monotonic() + 5
            while not self.closed.is_set() and time.monotonic() < deadline:
                try:
                    conn, _ = self.listener.accept()
                    break
                except socket.timeout:
                    continue
            else:
                return
            self.connection = conn
            self.connected.set()
            with conn:
                conn.settimeout(5)
                while True:
                    size = struct.unpack("!I", _exact(conn, 4))[0]
                    if size < 8 or size > 65536:
                        raise ValueError
                    startup = _exact(conn, size - 4)
                    protocol = struct.unpack("!I", startup[:4])[0]
                    if size == 8 and protocol in (80877103, 80877104):
                        conn.sendall(b"N")  # SSL/GSS negotiation is not a DB.
                        continue
                    if protocol != 196608:
                        raise ValueError
                    words = startup[4:].split(b"\0")
                    if words[-2:] != [b"", b""]:
                        raise ValueError
                    words = words[:-2]
                    if len(words) % 2:
                        raise ValueError
                    parameters = {}
                    for index in range(0, len(words), 2):
                        key, value = words[index:index+2]
                        if not key or key in parameters:
                            raise ValueError
                        parameters[key] = value
                    self.startup_user = parameters.get(b"user")
                    self.startup_database = parameters.get(b"database")
                    break
                if self.auth_mode == "none":
                    self.no_auth_offered.set()
                else:
                    conn.sendall(_packet(b"R", struct.pack("!I", 3)))
                    kind, payload = self._message(conn)
                    self.authenticated = kind == b"p" and payload == self.password + b"\0"
                    self.auth_received.set()
                    if not self.authenticated:
                        return
                    if self.pause and not self.release.wait(timeout=4):
                        raise TimeoutError
                conn.sendall(_packet(b"R", struct.pack("!I", 0)))
                for key, value in (
                    (b"server_version", b"16.0"),
                    (b"client_encoding", b"UTF8"),
                    (b"standard_conforming_strings", b"on"),
                    (b"integer_datetimes", b"on"),
                ):
                    conn.sendall(_packet(b"S", key + b"\0" + value + b"\0"))
                conn.sendall(_packet(b"K", struct.pack("!II", 731, 19)))
                conn.sendall(_packet(b"Z", b"I"))
                while not self.closed.is_set():
                    kind, payload = self._message(conn)
                    if kind == b"X":
                        return
                    if kind != b"Q" or payload.rstrip(b"\0 \t\r\n;") != b"SELECT 1":
                        raise ValueError
                    self.query_seen = True
                    field = b"?column?\0" + struct.pack("!IhIhih", 0, 0, 23, 4, -1, 0)
                    conn.sendall(_packet(b"T", struct.pack("!h", 1) + field))
                    conn.sendall(_packet(b"D", struct.pack("!hi", 1, 1) + b"1"))
                    conn.sendall(_packet(b"C", b"SELECT 1\0"))
                    conn.sendall(_packet(b"Z", b"I"))
        except (OSError, EOFError, ValueError, TimeoutError, struct.error):
            if not self.closed.is_set():
                self.failed = True
                self.auth_received.set()

    @staticmethod
    def _message(conn):
        kind = _exact(conn, 1)
        size = struct.unpack("!I", _exact(conn, 4))[0]
        if size < 4 or size > 1048576:
            raise ValueError
        return kind, _exact(conn, size - 4)


@unittest.skipUnless(sys.platform == "linux", "fleet Linux protocol control")
class AdapterControls(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        if not CLIENT.is_file() or CLIENT.read_bytes()[:4] != b"\x7fELF":
            raise unittest.SkipTest("required fixed fleet psql ELF is absent")
        actual_sha = hashlib.sha256(CLIENT.read_bytes()).hexdigest()
        if actual_sha != CLIENT_SHA256:
            raise AssertionError("the fixed fleet psql identity changed")
        cls.client_sha = CLIENT_SHA256

    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="synthetic-pgcall-", dir=ROOT)
        self.root = Path(self.temporary.name)
        self.addCleanup(self.temporary.cleanup)

    def source(self, body: str, name: str = "source.dsn") -> Path:
        path = self.root / name
        descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(descriptor, "w") as stream:
            stream.write(body)
        return path

    @staticmethod
    def dsn(port: int, *, password: bool = True, query: dict | None = None) -> str:
        userinfo = "fixture_user"
        if password:
            userinfo += ":" + urllib.parse.quote(SYNTHETIC_PASSWORD, safe="")
        options = {"sslmode": "disable"}
        options.update(query or {})
        return f"postgres://{userinfo}@127.0.0.1:{port}/fabric_fixture?" + urllib.parse.urlencode(options)

    def arguments(self, source: Path, *, client: Path = CLIENT, digest: str | None = None, extra=(), client_args=()):
        return [PYTHON, "-I", "-S", str(ADAPTER), "--client", str(client),
                "--client-sha256", digest or self.client_sha, "--purpose", "test",
                "--dsn-file", str(source), *extra, "--", "-X", "-A", "-t", "-q",
                "-v", "ON_ERROR_STOP=1", "--command", "SELECT 1", *client_args]

    def run_refusal(self, arguments, *, environment=None):
        try:
            result = subprocess.run(arguments, env=environment or dict(CLEAN_ENV),
                                    stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=5)
        except subprocess.TimeoutExpired:
            raise self.failureException("refusal exceeded its owned timeout") from None
        self.assertTrue(result.returncode != 0, "unsafe invocation was accepted")
        self.assertTrue(result.stdout == b"", "a refusal produced semantic output")
        output = result.stdout + result.stderr
        markers = (SYNTHETIC_PASSWORD.encode(), urllib.parse.quote(SYNTHETIC_PASSWORD, safe="").encode())
        self.assertFalse(any(marker in output for marker in markers), "a refusal echoed synthetic credential material")
        self.assertFalse(b"Traceback" in output, "a refusal exposed an internal traceback")
        return result

    def test_real_psql_auth_semantics_and_process_boundary(self):
        self._positive_control("dsn")

    def test_real_psql_password_file_auth_and_semantics(self):
        self._positive_control("password")

    def test_real_psql_named_field_auth_and_semantics(self):
        self._positive_control("field")

    def _positive_control(self, mode):
        with FakePostgres(pause=True) as backend:
            if mode == "password":
                password_file = self.source(SYNTHETIC_PASSWORD, "password")
                arguments = [PYTHON, "-I", "-S", str(ADAPTER), "--client", str(CLIENT),
                             "--client-sha256", self.client_sha, "--purpose", "test",
                             "--password-file", str(password_file), "--dsn", self.dsn(backend.port, password=False),
                             "--", "-X", "-A", "-t", "-q", "-v", "ON_ERROR_STOP=1", "--command", "SELECT 1"]
            elif mode == "field":
                source = self.source("FABRIC_TEST_DSN="+self.dsn(backend.port)+"\n", "field.env")
                arguments = self.arguments(source, extra=("--dsn-field", "FABRIC_TEST_DSN"))
            else:
                source = self.source(self.dsn(backend.port))
                arguments = self.arguments(source)
            extra_file = self.root / "unrelated-fd"
            extra_file.write_bytes(b"nonsecret descriptor control")
            with extra_file.open("rb") as unrelated:
                identity = os.fstat(unrelated.fileno())
                deadline = time.monotonic() + 5
                process = subprocess.Popen(arguments, env=dict(CLEAN_ENV),
                                           pass_fds=(unrelated.fileno(),), stdout=subprocess.PIPE,
                                           stderr=subprocess.PIPE)
                try:
                    self.assertTrue(backend.auth_received.wait(timeout=3), "actual psql did not reach synthetic authentication")
                    self.assertTrue(backend.authenticated, "actual psql did not load the exact synthetic password")
                    self.assertTrue(backend.startup_user == b"fixture_user" and
                                    backend.startup_database == b"fabric_fixture",
                                    "mock startup fields did not match the intended fixture identity")
                    self._check_process_boundary(process.pid, identity, backend.port)
                    backend.release.set()
                    stdout, stderr = process.communicate(timeout=max(0.1, deadline - time.monotonic()))
                    self.assertTrue(process.returncode == 0, "actual psql did not finish the semantic control")
                    self.assertTrue(stdout == b"1\n", "actual psql returned an unexpected row")
                    self.assertTrue(backend.query_seen and not backend.failed, "fake endpoint did not observe the known control query")
                    self.assertFalse(SYNTHETIC_PASSWORD.encode() in stderr, "actual client echoed synthetic credentials")
                finally:
                    backend.release.set()
                    if process.poll() is None:
                        process.kill()  # explicit Popen owned by this test only
                        process.communicate(timeout=1)

    def _check_process_boundary(self, pid, unrelated, port):
        process_root = Path("/proc") / str(pid)
        actual_executable, expected_executable = (process_root / "exe").stat(), CLIENT.stat()
        self.assertTrue((actual_executable.st_dev, actual_executable.st_ino) ==
                        (expected_executable.st_dev, expected_executable.st_ino),
                        "semantic endpoint was not reached by the fixed actual psql ELF")
        blocks = [(process_root / name).read_bytes() for name in ("cmdline", "environ")]
        markers = (SYNTHETIC_PASSWORD.encode(), urllib.parse.quote(SYNTHETIC_PASSWORD, safe="").encode())
        self.assertFalse(any(marker in block for block in blocks for marker in markers), "synthetic credential material reached process metadata")
        environment = dict(entry.split(b"=", 1) for entry in blocks[1].split(b"\0") if b"=" in entry)
        self.assertFalse(b"PGPASSWORD" in environment, "client received a password variable")
        self.assertTrue(environment.get(b"PGGSSENCMODE") == b"disable",
                        "client could consult ambient GSS credentials")
        self.assertTrue(environment.get(b"PGREQUIREAUTH") == b"password,md5,scram-sha-256",
                        "client did not require the reviewed password authentication methods")
        passfile = environment.get(b"PGPASSFILE", b"")
        self.assertTrue(passfile.startswith(b"/proc/self/fd/"), "client did not receive a descriptor passfile")
        try:
            number = int(passfile.rsplit(b"/", 1)[1])
        except ValueError:
            self.fail("client passfile descriptor is malformed")
        pass_path = process_root / "fd" / str(number)
        info = pass_path.stat()
        self.assertTrue(stat.S_ISREG(info.st_mode) and stat.S_IMODE(info.st_mode) == 0o600, "auth descriptor is not a private regular file")
        descriptor = os.open(pass_path, os.O_RDONLY)
        try:
            seals = fcntl.fcntl(descriptor, getattr(fcntl, "F_GET_SEALS", 1034))
            # This observer owns the synthetic source and actual child. Never
            # use this inspection against an operational credential FD.
            record = os.pread(descriptor, 4097, 0)
            prefix = f"127.0.0.1:{port}:fabric_fixture:fixture_user:".encode()
            self.assertTrue(len(record) <= 4096 and record.count(b"\n") == 1 and
                            record.endswith(b"\n"), "synthetic auth transport is not a single bounded record")
            self.assertTrue(record.startswith(prefix), "synthetic auth transport has unexpected connection scope")
            scope = record.split(b":", 4)[:4]
            self.assertTrue(len(scope) == 4 and all(field != b"*" for field in scope),
                            "synthetic auth transport contains wildcard scope")
        finally:
            os.close(descriptor)
        self.assertTrue(seals & 15 == 15, "auth descriptor is not sealed against mutation")
        unrelated_survived = False
        memfds = 0
        for entry in (process_root / "fd").iterdir():
            try:
                candidate = entry.stat()
                unrelated_survived |= candidate.st_dev == unrelated.st_dev and candidate.st_ino == unrelated.st_ino
                memfds += os.readlink(entry).startswith("/memfd:")
            except FileNotFoundError:
                continue
        self.assertFalse(unrelated_survived, "unrelated inherited descriptor survived exec")
        self.assertTrue(memfds == 1, "client inherited an unexpected RAM descriptor set")

    def test_actual_psql_rejects_no_authentication_endpoint(self):
        with FakePostgres(auth_mode="none") as backend:
            source = self.source(self.dsn(backend.port))
            try:
                result = subprocess.run(self.arguments(source), env=dict(CLEAN_ENV),
                                        stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=5)
            except subprocess.TimeoutExpired:
                raise self.failureException("no-auth control exceeded its owned timeout") from None
            self.assertTrue(backend.connected.is_set() and backend.no_auth_offered.is_set(),
                            "no-auth refusal lacked a positively armed synthetic endpoint")
            self.assertTrue(backend.startup_user == b"fixture_user" and
                            backend.startup_database == b"fabric_fixture",
                            "no-auth control did not reach the intended mock fixture identity")
            self.assertTrue(result.returncode != 0, "actual psql accepted a no-authentication endpoint")
            self.assertTrue(result.stdout == b"", "no-authentication endpoint produced semantic output")
            self.assertFalse(backend.query_seen, "client issued a query without password authentication")
            markers = (SYNTHETIC_PASSWORD.encode(), urllib.parse.quote(SYNTHETIC_PASSWORD, safe="").encode())
            self.assertFalse(any(marker in result.stdout+result.stderr for marker in markers),
                             "no-auth refusal echoed synthetic credential material")

    def test_metadata_canaries_are_not_exported(self):
        for marker in (SYNTHETIC_PASSWORD, urllib.parse.quote(SYNTHETIC_PASSWORD, safe="")):
            with self.subTest(encoded=marker != SYNTHETIC_PASSWORD), FakePostgres() as backend:
                path = self.source(self.dsn(backend.port, query={"application_name": marker}), name="metadata-"+str(len(marker)))
                self.run_refusal(self.arguments(path))
                self.assertFalse(backend.connected.is_set(), "rejected metadata reached authentication")

    def test_source_permissions_symlink_and_type_refuse(self):
        with FakePostgres() as backend:
            public = self.source(self.dsn(backend.port), "public.dsn")
            public.chmod(0o644)  # this test's newly created synthetic source only
            linked = self.root / "source-link"
            linked.symlink_to(public)
            for path in (public, linked, self.root):
                with self.subTest(kind=path.name):
                    self.run_refusal(self.arguments(path))
            self.assertFalse(backend.connected.is_set(), "unsafe source reached authentication")

    def test_wrong_hash_and_swapped_client_refuse(self):
        with FakePostgres() as backend:
            path = self.source(self.dsn(backend.port))
            self.run_refusal(self.arguments(path, digest="0" * 64))
            swapped = self.root / "swapped-client"
            swapped.write_bytes(b"\x7fELFsynthetic-not-the-reviewed-client")
            swapped.chmod(0o700)
            self.run_refusal(self.arguments(path, client=swapped))
            self.assertFalse(backend.connected.is_set(), "unverified client reached authentication")

    def test_connection_overrides_refuse(self):
        with FakePostgres() as backend:
            path = self.source(self.dsn(backend.port))
            for override in (("-d", "fabric_other"), ("--dbname=fabric_other",),
                             ("-h", "127.0.0.1"), ("-U", "fixture_other"),
                             ("--host=127.0.0.1",), ("-p", "5432"),
                             ("--service=synthetic",), ("-W",), ("-c", "SELECT 1")):
                with self.subTest(option=override[0]):
                    self.run_refusal(self.arguments(path, client_args=override))
            self.assertFalse(backend.connected.is_set(), "connection override reached authentication")

    def test_inherited_connection_and_startup_overrides_refuse(self):
        with FakePostgres() as backend:
            path = self.source(self.dsn(backend.port))
            # All values are synthetic. Clean startup must be established by
            # the trusted outer caller; adapter refusal alone cannot prove it.
            for name in ("PGPASSWORD", "PGSERVICE", "PGPASSFILE", "LD_PRELOAD", "PYTHONPATH"):
                environment = dict(CLEAN_ENV)
                environment[name] = "synthetic-override"
                with self.subTest(variable=name):
                    self.run_refusal(self.arguments(path), environment=environment)
            self.assertFalse(backend.connected.is_set(), "ambient override reached authentication")

    def test_source_modes_and_named_field_purpose_refuse(self):
        with FakePostgres() as backend:
            source = self.source(self.dsn(backend.port))
            password = self.source(SYNTHETIC_PASSWORD, "raw-password")
            self.run_refusal(self.arguments(source, extra=("--password-file", str(password), "--dsn", self.dsn(backend.port, password=False))))
            fields = self.source("FABRIC_DSN="+self.dsn(backend.port)+"\n", "fields.env")
            self.run_refusal(self.arguments(fields, extra=("--dsn-field", "FABRIC_DSN")))
            self.assertFalse(backend.connected.is_set(), "conflicting credential source reached authentication")

    def test_password_mode_userinfo_refuses(self):
        with FakePostgres() as backend:
            password = self.source(SYNTHETIC_PASSWORD, "raw-password")
            arguments = [PYTHON, "-I", "-S", str(ADAPTER), "--client", str(CLIENT),
                         "--client-sha256", self.client_sha, "--purpose", "test",
                         "--password-file", str(password), "--dsn", self.dsn(backend.port),
                         "--", "-X", "-A", "-t", "-q", "--command", "SELECT 1"]
            self.run_refusal(arguments)
            self.assertFalse(backend.connected.is_set(), "password-bearing metadata reached authentication")

    def test_controlled_exec_failure_closes_owned_descriptors(self):
        with FakePostgres() as backend:
            source = self.source(self.dsn(backend.port))
            helper = ROOT / "exec_failure_control.py"
            result = subprocess.run([PYTHON, "-I", "-S", str(helper), str(source)], env=dict(CLEAN_ENV),
                                    stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=5)
            self.assertTrue(result.returncode == 0 and result.stdout == b"EXEC_FAILURE_CLEANUP_OK\n",
                            "controlled exec failure did not close its owned auth/client descriptors")
            self.assertFalse(SYNTHETIC_PASSWORD.encode() in result.stdout+result.stderr,
                             "exec failure control echoed credential material")
            self.assertFalse(backend.connected.is_set(), "controlled failed exec reached authentication")

    def test_owner_metadata_refuses_before_source_read(self):
        with FakePostgres() as backend:
            source = self.source(self.dsn(backend.port))
            helper = ROOT / "exec_failure_control.py"
            result = subprocess.run([PYTHON, "-I", "-S", str(helper), str(source), "owner"], env=dict(CLEAN_ENV),
                                    stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=5)
            self.assertTrue(result.returncode == 0 and result.stdout == b"OWNER_METADATA_REFUSAL_OK\n",
                            "synthetic foreign-owner metadata was not refused before source read")
            self.assertFalse(backend.connected.is_set(), "owner-metadata unit control reached authentication")

    def test_non_profile_elf_has_safe_refusal(self):
        with FakePostgres() as backend:
            source = self.source(self.dsn(backend.port))
            broken = self.root / "broken-elf"
            broken.write_bytes(b"\x7fELFcontrolled-exec-failure")
            broken.chmod(0o700)
            digest = hashlib.sha256(broken.read_bytes()).hexdigest()
            self.run_refusal(self.arguments(source, client=broken, digest=digest))
            self.assertFalse(backend.connected.is_set(), "non-profile executable reached authentication")


if __name__ == "__main__":
    unittest.main(verbosity=2)
