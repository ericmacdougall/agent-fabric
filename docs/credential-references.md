# Database credential references

Linux clients can read an existing protected connection file inside the client
instead of receiving a loaded password through the environment. Set the
nonsecret `FABRIC_DSN_FILE` path for the application connection. A plain file
contains one PostgreSQL URL. For an existing environment-format data file,
also set `FABRIC_DSN_FILE_KEY=FABRIC_DSN` to select that field. The file is
parsed as data; it is never sourced as shell code.

The same interface applies to `FABRIC_TEST_DSN_FILE`,
`FABRIC_RUNNER_DSN_FILE` and `FABRIC_REVIEW_DSN_FILE`. A configurable runner
variable such as `--runner-dsn-env CUSTOM_RUNNER` uses `CUSTOM_RUNNER_FILE`,
but its allowed field remains `FABRIC_RUNNER_DSN`: changing a variable name
does not change the connection's purpose. Review connection values and file
metadata are removed from the worker parent environment after resolution.

When a role is provisioned from an existing raw password file, Linux clients
can reuse that exact source with `FABRIC_DSN_PASSWORD_FILE`. In this mode,
`FABRIC_DSN` contains only approved nonsecret connection metadata: a PostgreSQL
URL with username, server, database and supported options, with no password.
The client reads the protected raw source and builds the sealed exact passfile
inside its own process. It does not construct a password-bearing URL or write
another operational credential file. This also applies to test, runner and
review connection names and to configurable runner variable names.

An explicit nonsecret target such as `FABRIC_DSN_DATABASE` derives a new exact
reference before the first connection. It requires a declared file or
password-file source; an empty target or a target alongside only the legacy
raw variable is refused. The source database is never opened first. Host,
port, user and purpose are preserved, and the selected reference remains open
through pool shutdown and cleanup. This selector supplies no authority to
connect to a database: an operation still needs its own reviewed target scope.

The password-only mode is exclusive with the full DSN file and field selector.
It trims surrounding whitespace exactly as the role password loader does,
then refuses an empty effective password or embedded NUL/CR/LF. Connection
purpose labels supply no database authority: the actual expected server,
database, role identity and denied privileges still require authenticated
checks under the operation profile.

File mode requires a bounded regular file owned by the client user with no
group or other access. Path components are opened without following symbolic
links and must be controlled by the user or root. Root-owned sticky temporary
directories are permitted. Duplicate data fields, shell expressions, malformed
URLs, unknown connection options, conflicting raw variables and
ambient `PG*` settings are refused. The trusted operation launcher must establish
its clean environment **before** interpreter and dynamic-loader startup;
client-side checks do not replace that boundary.

Reference configuration is presence-aware. Setting `_FILE`, `_PASSWORD_FILE`,
`_FILE_KEY` or `_DATABASE` to an empty value is an error and never falls back to a raw
variable or skips a required test. Unset a mode to disable it. A full DSN file
cannot be combined with any raw variable presence; password-only mode requires
the nonempty, password-free metadata variable.

Supported URL options are `sslmode`, `connect_timeout` (1–120 seconds), and
the fixed versioned application names `fabd`, `fabd-install`, and `fab-tui`.
Arbitrary application metadata cannot be copied into a returned reference.
No default `.pgpass` file is created. These restrictions apply to file mode;
existing raw-variable callers retain their compatibility behavior. File mode
on unsupported platforms returns an explicit error. Darwin and Windows file
transport and runtime verification are not provided by this change.

`internal/connref.Ref` owns a sealed Linux memory file containing one exact
host, port, database and user passfile record. `DSN()` returns a secret-free
connection string whose lifetime is borrowed from that reference. Keep the
reference open until all pools, reconnects, child processes and deferred
cleanup using it have finished. `ForDatabase(name)` creates a new exact
record with independent ownership; changing the returned URL's database path
alone cannot authenticate to a different database. `Close()` is idempotent;
derivation from a closed reference fails. Existing children survive closing
their parent reference. Passfile matching supplies credentials, and does not
grant database privileges or authorize an operation.

Trusted children under the same PID view can use their parent's reference
while it remains open. A worker with a private PID namespace cannot use that
parent `/proc` reference. Disposable worker tests use scoped private fixture
passfiles with only the runner credential. This does not provide live worker
credential provisioning or change the deployed no-dispatch configuration.
Do not grant a worker access to an application/admin source or a combined
live environment file.

Child process boundaries remove source metadata, target selectors and raw
connection values before appending the child’s already resolved canonical
connection. A custom runner variable name does not exempt that variable or
its reference metadata from removal. This connection-specific cleanup is not
a replacement for the trusted launcher's clean environment boundary.

Operational credential sources and their contents belong outside the public
repository. Review packets identify code, executable inputs and opaque source
revisions; they never contain credential bytes or credential-content hashes.
Installation, database use, rehearsals and release remain separate reviewed
gates. Passing the synthetic reference tests is not live release evidence.

## Scope of this protection (goodqa#15661 F5)

The no-password-in-environment protection is opt-in: it applies only when a client
is started with `FABRIC_DSN_FILE` or `FABRIC_DSN_PASSWORD_FILE` (and the test,
runner and review variants). `tools/fabd-install/install.sh` still builds DSNs with
the password embedded and writes them to the service environment file, so the
installed service runs in the raw mode until a later reviewed change switches it.
The role password files read by `fab db roles` (`--*-password-file`) are read
without the owner and mode checks that connection reference files get; keep them
mode 0600 and owned by the operator. A full release that claims no password in an
environment must first move the installer to the reference modes.
