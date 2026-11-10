# Control Room database roles

`fab db roles --control-only --prefix PREFIX` applies three separate Control Room
roles. Supply each password through its own private file with
`--export-password-file`, `--ingest-password-file`, and `--service-password-file`.
An omitted file or a file whose trimmed contents are empty requires
`--allow-nologin`; confirmation applies to every role that would become NOLOGIN.
All three files are loaded and validated before database access or role changes.

The export role has whole-table SELECT on `public.events` and `public.outbox`,
and column SELECT on `public.missions(id, ticket)` and
`public.outbox_cursors(consumer, last_id)`. Those four metadata columns support the
existing ticket-reference and held-stream behavior of `fab control export`.
It cannot read other columns or `SELECT *` from those two metadata tables, write
tables, or access the Control Room schema. This is a narrow amendment to the
original events/outbox-only read contract, based on the actual exporter joins.

The ingest role can INSERT only the documented source-table columns and read
`control.allowed_sources`. The service role reads the source tables and its
named Fabric tables, and writes the projection tables. The committed literal
[`roles-matrix.json`](../internal/control/roles-matrix.json) independently states
the expected table and column grants. `--verify` compares actual privileges with
that matrix, including column drift; applying roles again removes grants made
by the managed grantor that are outside the contract.

Password-setting SQL runs on one acquired connection inside one transaction.
Before any role mutation, the password path disables activity tracking locally
and verifies it, alongside the server-log and utility-statement protections.
If protection cannot be established, the command refuses before mutation. These
settings apply to the transaction; no shared or global setting is changed.
After transaction completion, activity tracking must match the prior setting.
An uncertain transaction outcome or failed restoration check destroys the
connection before pool reuse. An error after confirmed commit explicitly reports
that roles committed; retry only after inspecting role state.

The runner safety check refuses any Control Room table or view privilege,
including column SELECT held directly or through an assumable NOINHERIT member.
A drifted unrelated Control Room role does not stop a clean runner or rollback.

PUBLIC rights, cross-database access restrictions, native Windows support, and
the SCRAM-verifier replacement remain separate open work. These changes do not
claim to close those targets.
