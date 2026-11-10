#!/usr/bin/env python3
"""Exec a pinned libpq client using an exact sealed RAM passfile.

This is credential transport, not authorization or a job supervisor. The trusted
managed caller must establish a clean environment BEFORE interpreter/loader
startup, freeze every code/SQL/profile input and bound the owned process group.
No credential contents, argv or environment are printed by this module.
"""
from __future__ import annotations

import argparse
import fcntl
import hashlib
import os
import re
import stat
import sys
import urllib.parse
from dataclasses import dataclass

MAX_SOURCE = 16_384
FIELDS = {"app": "FABRIC_DSN", "runner": "FABRIC_RUNNER_DSN", "test": "FABRIC_TEST_DSN", "review": "FABRIC_REVIEW_DSN"}
# This operational profile is reviewed with the actual installed client bytes.
# Caller-provided path/hash pairs cannot turn this transport into arbitrary exec.
CLIENTS = {
 "/usr/lib/postgresql/16/bin/psql": "df8031178589c31c7bc20aa2adf902e1cc3f0aa9ea12428aa5db3d476cbcac1a",
 "/usr/lib/postgresql/16/bin/pg_dump": "5abfee921d2efd952aa91912b033285820575df17840bb1faf69f8f9b0431e8d",
 "/usr/lib/postgresql/16/bin/pg_restore": "eb91db5f9ce7a1e4d8a72a9319544f5944ef00b6741c948b85e0a2ed532f9ad4",
}
APP_NAMES = frozenset({"fabd", "fabd-install", "fab-tui"})
GO_SPACE = "\t\n\v\f\r \u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000"


class Refusal(Exception):
    """Only fixed machine categories are emitted; input text never is."""


class Parser(argparse.ArgumentParser):
    def error(self, message):
        raise Refusal("invalid_arguments")


@dataclass(frozen=True)
class Connection:
    host: str
    port: str
    database: str
    user: str
    sslmode: str
    timeout: str | None
    application: str | None


def parser():
    p = Parser(add_help=False)
    p.add_argument("--client", required=True)
    p.add_argument("--client-sha256", required=True)
    p.add_argument("--purpose", choices=tuple(FIELDS), required=True)
    p.add_argument("--dsn-file")
    p.add_argument("--dsn-field")
    p.add_argument("--password-file")
    p.add_argument("--dsn")
    p.add_argument("--database")
    p.add_argument("--session", choices=("normal", "readonly"), default="normal")
    return p


def _field(value, limit=1024):
    return bool(value) and len(value.encode()) <= limit and not any(x in value for x in ("*", "\0", "\r", "\n"))


def _directory_ok(st):
    return st.st_uid in (0, os.geteuid()) and (not st.st_mode & 0o022 or (st.st_uid == 0 and st.st_mode & stat.S_ISVTX))


def _open_no_follow(path, flags):
    """Open relative to validated directory FDs; never reopen a checked path."""
    if not os.path.isabs(path) or os.path.normpath(path) != path:
        raise Refusal("invalid_reference")
    parts = path.removeprefix("/").split("/")
    directory = os.open("/", os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC)
    try:
        for part in parts[:-1]:
            if part in ("", ".", ".."):
                raise Refusal("invalid_reference")
            child = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC | os.O_NOFOLLOW, dir_fd=directory)
            os.close(directory)
            directory = child
            if not _directory_ok(os.fstat(directory)):
                raise Refusal("invalid_reference")
        if parts[-1] in ("", ".", ".."):
            raise Refusal("invalid_reference")
        return os.open(parts[-1], flags | os.O_CLOEXEC | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=directory)
    finally:
        os.close(directory)


def read_protected(path):
    fd = _open_no_follow(path, os.O_RDONLY)
    try:
        before = os.fstat(fd)
        if (not stat.S_ISREG(before.st_mode) or before.st_uid != os.geteuid()
                or stat.S_IMODE(before.st_mode) not in (0o400, 0o600)
                or before.st_nlink != 1 or not 0 < before.st_size <= MAX_SOURCE):
            raise Refusal("invalid_reference")
        pieces, size = [], 0
        while size <= MAX_SOURCE:
            data = os.read(fd, MAX_SOURCE + 1 - size)
            if not data:
                break
            pieces.append(data)
            size += len(data)
        after = os.fstat(fd)
        if (size > MAX_SOURCE or (before.st_size, before.st_mtime_ns, before.st_ctime_ns)
                != (after.st_size, after.st_mtime_ns, after.st_ctime_ns)):
            raise Refusal("invalid_reference")
        return b"".join(pieces).decode("utf-8", "strict")
    finally:
        os.close(fd)


def selected_dsn(body, field, purpose):
    if "\0" in body or "\r" in body or not body:
        raise Refusal("invalid_reference")
    if field is None:
        return body.removesuffix("\n")
    if not field or field != FIELDS[purpose]:
        raise Refusal("invalid_reference")
    seen, selected = set(), None
    for line in body.split("\n"):
        if not line or line.startswith("#"):
            continue
        name, sep, value = line.partition("=")
        if (not sep or not re.fullmatch(r"[A-Z][A-Z0-9_]{0,127}", name) or name in seen
                or any(x in value for x in "`$\"'\t ")):
            raise Refusal("invalid_reference")
        seen.add(name)
        if name == field:
            selected = value
    if not selected:
        raise Refusal("invalid_reference")
    return selected


def connection_metadata(dsn, supplied_password, database):
    if not dsn or dsn != dsn.strip() or any(x in dsn for x in ("\0", "\r", "\n", "\t")):
        raise Refusal("invalid_reference")
    if re.search(r"%(?![0-9A-Fa-f]{2})", dsn):
        raise Refusal("invalid_reference")
    u = urllib.parse.urlsplit(dsn)
    if u.scheme not in ("postgres", "postgresql") or u.fragment:
        raise Refusal("invalid_reference")
    password_present = u.password is not None
    if supplied_password and password_present or not supplied_password and not password_present:
        raise Refusal("invalid_reference")
    host = u.hostname or ""
    user = urllib.parse.unquote(u.username or "", errors="strict")
    db = database if database is not None else urllib.parse.unquote(u.path.removeprefix("/"), errors="strict")
    port = str(u.port or 5432)
    if (not _field(host) or "," in host or "/" in host or not _field(user)
            or not _field(db, 63) or not 1 <= int(port) <= 65535):
        raise Refusal("invalid_reference")
    values = {}
    for key, value in urllib.parse.parse_qsl(u.query, keep_blank_values=True, strict_parsing=True, errors="strict"):
        if key in values:
            raise Refusal("invalid_reference")
        if key == "sslmode" and value in ("disable", "allow", "prefer", "require", "verify-ca", "verify-full"):
            pass
        elif key == "connect_timeout" and value.isascii() and value.isdecimal() and 1 <= int(value) <= 120:
            pass
        elif key == "application_name" and value in APP_NAMES:
            pass
        else:
            raise Refusal("invalid_reference")
        values[key] = value
    connection = Connection(host, port, db, user, values.get("sslmode", "prefer"), values.get("connect_timeout"), values.get("application_name"))
    password = None if supplied_password else urllib.parse.unquote(u.password, errors="strict")
    if password is not None and any(x in password for x in ("\0", "\r", "\n")):
        raise Refusal("invalid_reference")
    return connection, password


def load_connection(args):
    # None means absent; an explicit empty option is invalid, never fallback.
    if args.password_file is not None:
        if not args.password_file or not args.dsn or args.dsn_file is not None or args.dsn_field is not None:
            raise Refusal("invalid_reference")
        connection, _ = connection_metadata(args.dsn, True, args.database)
        password = read_protected(args.password_file).strip(GO_SPACE)
        if not password or any(x in password for x in ("\0", "\r", "\n")):
            raise Refusal("invalid_reference")
        return connection, password
    if not args.dsn_file or args.dsn is not None:
        raise Refusal("invalid_reference")
    if args.dsn_field is not None and (not args.dsn_field or args.dsn_field != FIELDS[args.purpose]):
        raise Refusal("invalid_reference")
    dsn = selected_dsn(read_protected(args.dsn_file), args.dsn_field, args.purpose)
    return connection_metadata(dsn, False, args.database)


def verified_client(path, expected):
    tool = os.path.basename(path)
    if path not in CLIENTS or expected != CLIENTS[path]:
        raise Refusal("invalid_client")
    fd = _open_no_follow(path, os.O_RDONLY)
    try:
        before = os.fstat(fd)
        if (not stat.S_ISREG(before.st_mode) or before.st_uid not in (0, os.geteuid())
                or stat.S_IMODE(before.st_mode) & 0o022 or not before.st_mode & 0o111):
            raise Refusal("invalid_client")
        digest = hashlib.sha256()
        prefix = os.read(fd, 4)
        digest.update(prefix)
        while data := os.read(fd, 131072):
            digest.update(data)
        after = os.fstat(fd)
        if (prefix != b"\x7fELF" or digest.hexdigest() != expected
                or (before.st_size, before.st_mtime_ns, before.st_ctime_ns)
                != (after.st_size, after.st_mtime_ns, after.st_ctime_ns)):
            raise Refusal("invalid_client")
        if os.execve not in os.supports_fd:
            raise Refusal("unsupported_exec")
        return fd, tool
    except BaseException:
        os.close(fd)
        raise


def client_arguments(tool, items, database):
    # Only options used by the reviewed operational clients are supported.
    # SQL and archive paths remain trusted caller inputs in its frozen closure.
    flags = {
        "psql": {"-X", "--no-psqlrc", "-A", "--no-align", "-t", "--tuples-only", "-q", "--quiet", "-At", "-Atq"},
        "pg_dump": {"-Fc", "--format=custom", "--no-owner", "--no-acl", "--schema-only", "--data-only"},
        "pg_restore": {"--exit-on-error", "--no-owner", "--no-acl", "--clean", "--if-exists", "--list", "-l"},
    }[tool]
    takes = {
        "psql": {"--command", "-v", "--set"},
        "pg_dump": {"-f", "--file", "--schema", "--exclude-schema", "--table", "--exclude-table"},
        "pg_restore": {"-f", "--file", "--schema", "--table"},
    }[tool]
    result, index, archive = [], 0, False
    while index < len(items):
        value = items[index]
        if "\0" in value:
            raise Refusal("invalid_arguments")
        if value in flags:
            result.append(value)
        elif value in takes:
            index += 1
            if index >= len(items) or not items[index] or "\0" in items[index]:
                raise Refusal("invalid_arguments")
            argument = items[index]
            if value in ("-v", "--set") and argument != "ON_ERROR_STOP=1":
                raise Refusal("invalid_arguments")
            if tool == "psql" and value not in ("-v", "--set") and argument.lstrip().startswith("\\"):
                raise Refusal("invalid_arguments")
            result.extend((value, argument))
        elif tool == "pg_restore" and not value.startswith("-") and os.path.isabs(value) and not archive:
            result.append(value)
            archive = True
        else:
            raise Refusal("invalid_arguments")
        index += 1
    if tool == "psql":
        if "--command" not in items:
            raise Refusal("invalid_arguments")
        result = ["--no-psqlrc", "--no-password", "--pset=pager=off"] + result
    else:
        result.insert(0, "--no-password")
        if tool == "pg_restore" and not any(x in items for x in ("--list", "-l")):
            result.insert(1, "--dbname=" + database)
    return result


def authentication_fd(connection, password):
    def escape(value):
        return value.replace("\\", "\\\\").replace(":", "\\:")
    record = ":".join(escape(x) for x in (connection.host, connection.port, connection.database, connection.user, password)) + "\n"
    fd = os.memfd_create("fabric-libpq-auth", os.MFD_CLOEXEC | os.MFD_ALLOW_SEALING)
    try:
        os.fchmod(fd, 0o600)
        data = memoryview(record.encode())
        while data:
            count = os.write(fd, data)
            if not count:
                raise Refusal("transport_failed")
            data = data[count:]
        fcntl.fcntl(fd, fcntl.F_ADD_SEALS, fcntl.F_SEAL_WRITE | fcntl.F_SEAL_GROW | fcntl.F_SEAL_SHRINK | fcntl.F_SEAL_SEAL)
        os.set_inheritable(fd, True)  # exactly this authentication FD survives exec
        return fd
    except BaseException:
        os.close(fd)
        raise


def child_environment(connection, auth_fd, session):
    env = {"PATH": "/usr/bin:/bin", "LANG": "C.UTF-8", "PGHOST": connection.host,
           "PGPORT": connection.port, "PGUSER": connection.user, "PGDATABASE": connection.database,
           "PGSSLMODE": connection.sslmode, "PGPASSFILE": "/proc/self/fd/" + str(auth_fd),
           "PGGSSENCMODE": "disable", "PGREQUIREAUTH": "password,md5,scram-sha-256"}
    if connection.timeout is not None:
        env["PGCONNECT_TIMEOUT"] = connection.timeout
    if connection.application is not None:
        env["PGAPPNAME"] = connection.application
    if session == "readonly":
        env["PGOPTIONS"] = "-c default_transaction_read_only=on"
    return env


def close_unrelated(keep):
    for name in os.listdir("/proc/self/fd"):
        if name.isdecimal() and int(name) > 2 and int(name) not in keep:
            try:
                os.close(int(name))
            except OSError:
                pass  # the directory iterator FD may already have closed


def execute(args, items):
    # This check cannot undo loader/interpreter startup. The managed caller
    # must already have established the clean pre-start environment.
    if any(name.startswith(("PG", "LD_", "PYTHON")) or name in ("BASH_ENV", "ENV") for name in os.environ):
        raise Refusal("inherited_environment")
    client_fd = auth_fd = None
    try:
        client_fd, tool = verified_client(args.client, args.client_sha256)
        client_arguments(tool, items, "")  # reject unsupported flags before reading credentials
        connection, password = load_connection(args)
        argv = [args.client] + client_arguments(tool, items, connection.database)
        auth_fd = authentication_fd(connection, password)
        env = child_environment(connection, auth_fd, args.session)
        close_unrelated({client_fd, auth_fd})
        os.execve(client_fd, argv, env)  # same open executable identity verified above
        raise Refusal("exec_failed")
    finally:
        for fd in (auth_fd, client_fd):
            if fd is not None:
                try:
                    os.close(fd)
                except OSError:
                    pass


def main(argv=None):
    try:
        if sys.platform != "linux":
            raise Refusal("unsupported_platform")
        if not sys.flags.isolated or not sys.flags.no_site:
            raise Refusal("unsafe_interpreter_startup")
        words = list(sys.argv[1:] if argv is None else argv)
        if "--" not in words:
            raise Refusal("invalid_arguments")
        split = words.index("--")
        args = parser().parse_args(words[:split])
        execute(args, words[split + 1:])
    except Refusal as error:
        category = str(error)
        if not re.fullmatch(r"[a-z_]{1,40}", category):
            category = "operation_failed"
        sys.stderr.write("pgcall-ref: " + category + "\n")
        return 2
    except (OSError, ValueError, UnicodeError, OverflowError):
        sys.stderr.write("pgcall-ref: operation_failed\n")
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
