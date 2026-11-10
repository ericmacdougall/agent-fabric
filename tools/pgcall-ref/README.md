# Fixed libpq credential references

`pgcall_ref.py` replaces its process with one of the reviewed PostgreSQL16
client executables. It validates and executes the same open ELF descriptor,
and retains one sealed0600memory passfile across that exec. Loaded passwords
never enter the client arguments or environment. The fixed executable profile
contains exact paths and digests; a caller cannot select an arbitrary program
by naming it `psql` or supplying another path/hash pair.

This is transport for trusted operational callers. It grants no authority,
does not supervise jobs, and does not make arbitrary SQL safe. The surrounding
managed operation must freeze its executable/runtime/source/SQL/profile inputs,
establish the clean child environment before interpreter/loader startup, impose
a finite owned lifecycle, verify the expected server/database/role and preserve
its ordinary locks, busy checks and rollback criteria. Database credentials are
read only after the fixed client and supported argument form have validated.

Invoke with `/usr/bin/python3 -I -S`. The script refuses a non-isolated or
site-enabled interpreter, ambient `PG*`, `LD_*`, `PYTHON*`, shell startup settings,
unsupported client/connection options and arbitrary application metadata. A
post-start refusal does not undo injected startup code: the trusted caller must
already have supplied an explicit clean environment.

Sources match the native reference contract:

- `--dsn-file` reads a protected PostgreSQL URL. Add `--dsn-field` only to
  select the canonical field for the explicit `--purpose` from an existing
  environment-format **data** file. It never sources that file as shell code.
- `--password-file` reuses an existing protected raw source with `--dsn`
  containing only nonsecret metadata. It uses the same effective whitespace
  trimming as the native role loader, then refuses empty/NUL/CR/LF values.
- Source modes are exclusive and explicit empty options refuse without fallback.
  `--database` derives an exact record for an explicitly selected database; this
  matching operation does not grant database privileges.

The resulting passfile has one exact host/port/database/user record. No default
or persistent `.pgpass` is created. All unrelated descriptors are closed; the
client owns the auth descriptor until exit. If exec fails, both owned descriptors
are closed and only a fixed failure category is emitted. `--session readonly`
sets the fixed read-only startup option; arbitrary connection options are refused.

Only the argument forms needed by reviewed callers are supported. Use the
documented `psql --command` option; `-c`, connection overrides, interactive/stdin
psql commands and leading psql meta-commands are refused. Normal SQL backslash
literals remain supported. Dumps/restores and SQL can carry private data, so their
actual sources, destinations and logging still belong to the reviewed operation.
Changing an installed client digest requires a new reviewed profile, never an
automatic trust update.

The child disables ambient GSS encryption negotiation and requires one of the
password authentication methods. A server that skips the password challenge
cannot supply password-authentication evidence. This avoids relying on an
ambient Kerberos cache for the password-source route; it changes no server
configuration. TLS and any additional credential inputs still require their
own declared operational profile. See the PostgreSQL16
[connection parameters](https://www.postgresql.org/docs/16/libpq-connect.html)
and [environment settings](https://www.postgresql.org/docs/16/libpq-envars.html).

The focused Linux control uses an in-process PostgreSQL protocol double and the
actual fixed `psql` executable:

```
/usr/bin/python3 -I -S tools/pgcall-ref/test_pgcall_ref.py
```

It starts no PostgreSQL server and executes no database SQL. It verifies the
running executable identity, synthetic authentication, exact startup metadata,
the known row response, sealed exact auth record, process metadata and descriptor
lifecycle in all three source modes. Negative controls cover metadata/source/
client/argument overrides and exec-failure cleanup. The foreign-owner check is
a unit fault control; it does not claim an actual account/chown integration test.

These controls are Linux/profile-specific. They do not certify real database
authentication, full pg_dump/pg_restore flows, copied-data rehearsal, deployment,
platform runtime or live release. Those checks remain separate gates.
