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
- `subject.ticket` is the mission's ticket reference as `project#N` (for example
  `goodqa#15586`), lower-cased, taken from `missions.ticket` (the first match of a bounded `project#N`: letters, digits and hyphens, at most 7 digits, not inside a longer word or number, which is the shape Mission Control accepts); no other ticket text is
  exported, and the field is absent when the mission has no such reference. It is
  Mission Control's join key (initiatives there are keyed by ticket).
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
  and the exit code stays 0. Because a file-drop reader never sees stderr, the stream
  also carries one event `fabric:lag:<first held outbox id>` (kind `system`, severity
  `warn`, summary `fabric export lagging: N rows held back`), stable per lag episode; the
  next normal event clears it. N is the count when that call ran; the id is keyed on the
  first held row only, so a consumer that dedupes on id keeps the first summary of an
  episode. The cursor does not move past them, so a later call returns
  them in order.
- A held-stream placeholder (`stream held, event withheld`) is exported once under its
  `fabric:<id>`. If the hold is later released, the same id is not re-exported with the
  real kind; a consumer that dedupes on id keeps the placeholder.
- A backfill is marked `"historic":true` on every event: a call without `--since`, and
  each following call of that cursor chain until a call catches up (the cursor records
  the mode). Both an output cap (`More`) and known held rows (`Lagging > 0`) keep
  it in backfill mode. Lag notices inherit that mode. Events after verified
  catch-up are live and carry no `historic` field.
- Rows come from one `REPEATABLE READ, READ ONLY` transaction and only when their
  transaction is older than every in-progress one (`o.xid < pg_snapshot_xmin(...)`),
  which with the xid-ordered `outboxOrderLock` (#15577) means a late commit is never
  skipped by the cursor.

## Cursor upgrades and binary rollback

New cursors use `fx2` for the mode-bearing layout. Both published `fx1` layouts
(original three-field body and later four-field body) are accepted after an
upgrade, at the same database/row position, with the usual database and row-content
checks. Their remaining rows are conservatively historic until catch-up: an old
four-field live bit may have come from the stalled-backfill defect. This can
suppress a pulse during the first upgraded catch-up; it does not skip rows.

`fab control cursor-rollback CURSOR` converts a valid caught-up `fx2` token to
the original three-field `fx1` layout without a database connection. It refuses
backfill, legacy and malformed tokens with exit3 and no stdout. A backfill must
first drain under this version until both More and Lagging are zero. Conversion
does not authenticate a database or establish a consumer's ability to seed the
token; the exporter checks database identity and row content on actual resume.

The installed Mission Control collector keeps an in-memory cursor and provides
no initial-cursor seeding interface. For that consumer, pause Fabric collection
before reverting to an incompatible exporter and keep it paused until compatible
code returns. Do not reset/replay through an old binary while the live consumer
is collecting: that can pulse historical events. The converter supports consumers
with a separately tested seeding path; it is not a working seeding procedure for
the installed collector. Binary-only Fabric rollback is still available; the
export consumer stays paused independently.

Mission Control's installed adapter also currently passes `--since 0` initially
and its external-event normalizer drops `historic`. Those consumer integration
gaps need their own repair and live proof before end-to-end pulse safety can be
claimed; this producer change does not modify the harness or its live install.

## Tests

On a machine with Postgres: `FABRIC_TEST_DSN=<admin dsn of a disposable instance> go test ./internal/control/...`.
`storetest` creates and drops its own `fabric_ctlx_<pid>` database. Failing-first controls
(payload leak, missing xid filter, no cap, read-write transaction, no stale check, missing
`fabric:` prefix) each fail the tests named in the PR.
