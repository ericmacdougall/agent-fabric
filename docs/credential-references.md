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

File mode requires a bounded regular file owned by the client user with no
group or other access. Path components are opened without following symbolic
links and must be controlled by the user or root. Root-owned sticky temporary
directories are permitted. Duplicate data fields, shell expressions, malformed
URLs, unknown connection options, conflicting nonempty raw variables and
ambient `PG*` settings are refused. The trusted operation launcher must establish
its clean environment **before** interpreter and dynamic-loader startup;
client-side checks do not replace that boundary.

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

Operational credential sources and their contents belong outside the public
repository. Review packets identify code, executable inputs and opaque source
revisions; they never contain credential bytes or credential-content hashes.
Installation, database use, rehearsals and release remain separate reviewed
gates. Passing the synthetic reference tests is not live release evidence.
