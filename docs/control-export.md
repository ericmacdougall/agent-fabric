# fab control export (goodqa#15586 B2)

`fab control export [--since CURSOR]` prints the fabric outbox as `mc.event/1`
lines (schema pinned at agent-harness d825d66, `docs/schemas/mc-event-v1.schema.json`,
blob d7506f0d224dd83d9105ef2f8f443f5fe5efb86a) for Mission Control (goodqa#15585).
It needs `FABRIC_DSN` and runs as the app role. It is read-only.

## Output

One JSON object per line, then exactly one `{"cursor":"OPAQUE"}` line.

- `id` is `fabric:<outbox id>`, stable across calls.
- `ts` is the event time (UTC, milliseconds, `Z`).
- `subject.initiative` is the stream: the fabric mission id. (Assumption: "stream"
  means mission, as in `fab streams held`.)
- `kind` is `owner` for mission, attempt, candidate and effect events, `release` for
  DELIVERED, `deploy` for LIVE_VERIFIED, and `system` for wake, ladder and dispatch
  notices, for a held stream, and for any kind this build does not know.
- `summary` is at most 280 characters: a fixed phrase, the mission id and the sequence
  number, e.g. `mission m_x: attempt started (seq 3)`. The event payload is never
  selected from the database, so none of its text can reach a line. Held and unknown
  events read `stream held, event withheld` or `event of an unrecognized kind withheld`.

## Cursor and limits

- Omit `--since` to start at the beginning. The first page of a long history is capped:
  output is at most 1 MiB, never cut mid-line, and stderr says so; call again with the
  printed cursor until a call prints no event lines and stderr shows no LAGGING line.
- A call with no new events exits 0 with only the cursor line (the same cursor).
- A cursor is refused with exit 3 and nothing on stdout when it is malformed, was minted
  for another database (different name or oid), names an outbox row this store does not
  have (stale), or names a row whose content differs (a rebuilt store or another cluster
  with the same database name and oid: the cursor carries a hash of the row's mission,
  seq and insert time).
- Lagging: a committed row is held back while any older transaction in the cluster is
  open (this is the same visibility rule the scheduler uses). "No event lines" therefore
  does not always mean caught up. When rows are held back, stderr says `LAGGING: N ...`
  and the exit code stays 0; the cursor does not move past them, so a later call returns
  them in order.
- A held-stream placeholder (`stream held, event withheld`) is exported once under its
  `fabric:<id>`. If the hold is later released, the same id is not re-exported with the
  real kind; a consumer that dedupes on id keeps the placeholder.
- A backfill is marked `"historic":true` on every event: a call without `--since`, and
  each following call of that cursor chain until a call catches up (the cursor records
  the mode). Events after that are live and carry no `historic` field.
- Rows come from one `REPEATABLE READ, READ ONLY` transaction and only when their
  transaction is older than every in-progress one (`o.xid < pg_snapshot_xmin(...)`),
  which with the xid-ordered `outboxOrderLock` (#15577) means a late commit is never
  skipped by the cursor.

## Tests

On a machine with Postgres: `FABRIC_TEST_DSN=<admin dsn of a disposable instance> go test ./internal/control/...`.
`storetest` creates and drops its own `fabric_ctlx_<pid>` database. Failing-first controls
(payload leak, missing xid filter, no cap, read-write transaction, no stale check, missing
`fabric:` prefix) each fail the tests named in the PR.
