# Control-role password SQL boundary

Both the control-role and Store App/Runner apply operations use a fresh physical connection from the
reviewed stock `Store.Open` configuration. It never returns that connection to a
pool. Unsupported pool policy callbacks, query tracers and raw notice/error or
notification callbacks refuse before password transmission; they are not
silently removed. Stock pgx authentication, TLS and supported target-session
validation remain in force. Callback identity checks are pinned to pgx5.9.2 and
the reviewed Store factory; this is not a sandbox for arbitrary caller-owned
function closures or a malicious client process.

Passwords are absent from client SQL text. After validating all role/password
inputs, the operation verifies its approved PostgreSQL16.15 runtime/module
profile and native `stderr` destination. It then sets and verifies transaction
local activity, logging, verbosity, parameter-length and debug protections, plus
collection/utility protection for an approved pg_stat_statements installation.
Unknown destinations, preloads, extensions and unsupported module profiles
refuse; no unknown intentional audit hook is disabled to force success.

A cryptographically random128bit custom-GUC namespace is checked for collision,
preclaimed with an empty local value and verified. The password crosses only an
explicit `pgx.QueryExecModeExec` extended Bind to a catalog-qualified
`set_config` boolean projection. Its text value is never a query result. A
password-free `DO` body performs one fixed `ALTER ROLE`, using catalog-qualified
`format` with `%I`/`%L` and `current_setting`. Role attributes, LOGIN and password
change together at the original ALTER position; the handoff is cleared
immediately afterward. Empty passwords retain the explicit NOLOGIN behavior.
The grant matrix and outer transaction remain unchanged.

Only SQLSTATE reaches client diagnostics from an underlying PostgreSQL error;
message/detail/context/internal-query/Bind arguments are discarded. Cleanup
checks the empty handoff and restored local settings when possible, then closes
the dedicated connection in all paths. Cancellation does not imply that no write
occurred. An unconfirmed commit is an operational error; confirmed commit followed
by failed readback/cleanup explicitly reports committed state.

The protection assumes a trusted stock backend and reviewed module/image/crash
profile. It cannot protect against a malicious server or privileged memory
inspection and makes no zeroization claim. Native CSV/JSON sinks are refused;
Docker's capture driver is a separate layer. `terse` stderr handling and the
single nested utility source proof do not themselves prove crash confidentiality.
Root must verify the actual Apport handler/config, PID/mount namespace, exact
container identity and socket absence before any operational source connection.
Core limit0 alone does not prove suppression of a piped crash handler.

Pure protocol-double controls prove the old literal path exposes a public fixture
marker in Query SQL, while the repaired path carries it only in Bind despite a
caller SimpleProtocol default. Other focused controls cover raw frontend error
fields, collision, invalid inputs, setting failure/restoration and unsupported
callbacks/modules/sinks. They do not execute the existing control package's DB
TestMain or replace actual database/authentication acceptance.

Actual16.15 row-lock/observer, successful/failing/cancelled/uncertain transaction,
statistics, roles/grants and crash tests remain separate integration gates. The
sensitive observer uses backend PID and `pg_blocking_pids`, not a query tracer.
A genuine nested-utility PANIC requires a separately reviewed disposable privacy
fixture, bounded memory-only diagnostic observer and test-only fault module. The
production module guard must still refuse that module; no test exception is
permitted in production. Backend termination and RAISE EXCEPTION do not establish
the PANIC property. The shared channel has two closed builders: control roles retain NOINHERIT;
Store adds neither INHERIT nor NOINHERIT and preserves its existing behavior,
memberships and Scheduler NOLOGIN/no-password state. The separate SCRAM verifier
replacement remains open.

Source: accepted narrow password-SQL contract, GoodQA#15623/#15655. PostgreSQL's
[DO documentation](https://www.postgresql.org/docs/16/sql-do.html) describes the
anonymous block and language placement; its
[logging documentation](https://www.postgresql.org/docs/16/runtime-config-logging.html)
distinguishes native destinations, verbosity and collector behavior. Live
provisioning and completion are not established by this document.
