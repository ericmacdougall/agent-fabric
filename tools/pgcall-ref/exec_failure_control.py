"""Owned-process synthetic fault control; never a credential execution route."""
from __future__ import annotations

import errno
import importlib.util
import os
from pathlib import Path
import sys

CLIENT = "/usr/lib/postgresql/16/bin/psql"
CLIENT_SHA256 = "df8031178589c31c7bc20aa2adf902e1cc3f0aa9ea12428aa5db3d476cbcac1a"


def load_adapter():
    module_path = Path(__file__).resolve().parent / "pgcall_ref.py"
    spec = importlib.util.spec_from_file_location("synthetic_pgcall_ref", module_path)
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    # Execute this exact source read without consulting or creating bytecode
    # caches outside the declared test/adapter source closure.
    exec(compile(module_path.read_bytes(), str(module_path), "exec"), module.__dict__)
    return module


def control():
    module = load_adapter()
    tracked = {}
    verified_client = module.verified_client
    authentication_fd = module.authentication_fd

    def observe_client(*arguments):
        fd, name = verified_client(*arguments)
        tracked["client"] = fd
        return fd, name

    def failed_exec(fd, _arguments, _environment):
        if fd != tracked.get("client"):
            raise AssertionError
        tracked["exec_attempted"] = True
        raise OSError(errno.ENOEXEC, "synthetic exec failure")

    def observe_auth(*arguments):
        fd = authentication_fd(*arguments)
        tracked["auth"] = fd
        # Install only after actual client verification checked supports_fd.
        # This process is disposable; no parent process/module is patched.
        module.os.execve = failed_exec
        return fd

    module.verified_client = observe_client
    module.authentication_fd = observe_auth
    arguments = module.parser().parse_args([
        "--client", CLIENT, "--client-sha256", CLIENT_SHA256, "--purpose", "test",
        "--dsn-file", sys.argv[1],
    ])
    try:
        module.execute(arguments, ["-X", "-A", "-t", "-q", "--command", "SELECT 1"])
    except OSError:
        pass
    else:
        raise AssertionError
    if not tracked.get("exec_attempted") or "auth" not in tracked or "client" not in tracked:
        raise AssertionError
    for key in ("auth", "client"):
        try:
            os.fstat(tracked[key])
        except OSError as error:
            if error.errno != errno.EBADF:
                raise AssertionError
        else:
            raise AssertionError
    sys.stdout.write("EXEC_FAILURE_CLEANUP_OK\n")


def owner_control():
    module = load_adapter()
    source_identity = os.stat(sys.argv[1])
    real_fstat, real_read = module.os.fstat, module.os.read
    observed = {"foreign_metadata": False, "source_reads": 0}

    def foreign_owner(fd):
        info = real_fstat(fd)
        if (info.st_dev, info.st_ino) == (source_identity.st_dev, source_identity.st_ino):
            values = list(info)
            values[4] = 0 if os.geteuid() != 0 else 1
            observed["foreign_metadata"] = True
            return os.stat_result(values)
        return info

    def observe_read(fd, count):
        info = real_fstat(fd)
        if (info.st_dev, info.st_ino) == (source_identity.st_dev, source_identity.st_ino):
            observed["source_reads"] += 1
        return real_read(fd, count)

    module.os.fstat, module.os.read = foreign_owner, observe_read
    try:
        try:
            module.read_protected(sys.argv[1])
        except module.Refusal:
            pass
        else:
            raise AssertionError
    finally:
        module.os.fstat, module.os.read = real_fstat, real_read
    if not observed["foreign_metadata"] or observed["source_reads"] != 0:
        raise AssertionError
    sys.stdout.write("OWNER_METADATA_REFUSAL_OK\n")


if __name__ == "__main__":
    try:
        if len(sys.argv) == 3 and sys.argv[2] == "owner":
            owner_control()
        else:
            control()
    except Exception:
        sys.stderr.write("synthetic exec failure control failed\n")
        raise SystemExit(1)
