# fab control ingest (goodqa#15586 B3)

`fab control ingest --source NAME < events.jsonl` validates `mc.event/1` lines from an
external producer (schema pinned at agent-harness d825d66, blob d7506f0d) and stores them
in `control.ingested_events`. It needs `FABRIC_DSN` and the control schema
(created once by the database owner through the control schema bootstrap, owned by the control schema change, not by this one). It writes that one
table only: never fabric `events`, `outbox` or any other table.

## Contract

- The producer is pinned by `--source` (`^[a-z][a-z0-9_-]{0,31}$`), never by the line.
  `fabric` is reserved for `fab control export` and refused (exit 3). Every `id` must start
  with `NAME:` and match the schema pattern. A line with an `id` starting `fabric:` (any
  case) is rejected as `reserved-id-prefix`. A `source` field in a line must equal NAME.
- A line is rejected, with a reason code and its line number, when: it is not one JSON
  object, has trailing data or a duplicate key, is not valid UTF-8, exceeds 64 KiB, has a
  version other than `mc.event/1`, an unknown top-level, `actor` or `subject` field, a bad
  `id`, `ts`, `kind`, `severity` or `count`, a string over its limit, or any control
  character (ESC and so ANSI, NUL, newline, C1), line or paragraph separator, bidi override
  or BOM in any text field. Reasons are fixed codes; input text is never echoed.
- Secrets are redacted before storage, because the table is append-only and a stored
  credential could never be removed. Every free-text value (summary, host, tenant, actor
  and subject fields) has secret-shaped spans replaced by `[REDACTED:<kind>]`, using the
  shapes of the harness's shared list (agent-harness d825d66 `harness/hooks/redact.py`: API
  keys and tokens, JWT, Bearer headers, URL userinfo, PEM blocks, NAME=value credentials).
  A redacted summary is cut back to 280 characters. An `id` that itself looks like a
  credential is refused (`secret-in-id`). The stderr line reports `redacted-fields=N`.
  Redaction favours recall: a harmless string may gain a placeholder.
- Invisible format characters (zero-width space, direction marks, tag characters) are
  rejected with the other control characters. A JSON `null` for an optional field is
  rejected (`bad-type:<field>`).
- Rows are inserted in id order, so two concurrent calls with overlapping ids cannot
  deadlock.
- The CLI reads and validates stdin before it connects, so a stalled pipe never holds a
  database connection.
- Not enforced here: that the caller may write as `--source`. Anyone holding `FABRIC_DSN`
  can claim any producer name, and the first write of an id is permanent. The grants
  slice must bind source to the database role (tracked in the B3 follow-up ticket).
- `ts` follows the schema pattern (RFC 3339 UTC `Z`, fraction optional up to 6 digits) and
  is stored at millisecond precision.
- Storage mapping: `actor` is the first of `actor.agent`, `actor.session`, `actor.runtime`;
  `initiative`, `repo`, `branch`, `ref`, `ticket` come from `subject`. Fields the table
  bounds more tightly than the schema are cut to the column limit (actor and initiative 128,
  ticket 64, count 1,000,000), never refused.

## Limits and results

- Per call: 1 MiB, 2000 lines, 64 KiB per line. Over the byte or line limit the whole call
  is refused (exit 3) before anything is written. Empty lines count toward the line limit.
- A malformed line is rejected alone; the rest continue. Counts go to stderr:
  `ingest: source=S lines=N accepted=A inserted=I existing=E duplicate-in-input=D rejected=R`,
  then a count per reason and up to 20 `line L rejected: reason` lines.
- Exit codes: `0` every line was stored or already present; `4` the call completed and the
  accepted lines are stored, but some lines were rejected (counts on stderr); `3` the call
  was refused and nothing was written (bad usage, reserved or invalid source, over a limit);
  `1` an error of unknown effect (for example the database). A pipeline treats `4` as
  "inspect the rejects", not as a failure to retry.
- Idempotent by id: `INSERT ... ON CONFLICT (id) DO NOTHING`; the first write wins, a
  replay with changed content changes nothing. One transaction per call: a database error
  leaves nothing behind. After the commit, and only when a row was inserted, the call sends
  an empty `NOTIFY control_events`.

## Tests

`FABRIC_TEST_DSN=<admin dsn of a disposable instance> go test ./internal/control/ingest/`
(needs the control schema package from the control schema PR; `storetest` creates and drops
its own `fabric_ctli_<pid>` database). `testdata/vectors.jsonl` holds the shared vectors:
the first group mirrors `harness/mission_control/events.py` `validate()` at d825d66, the
second the stricter rules above. Failing-first controls: skipping the `fabric:` check,
dropping `ON CONFLICT`, writing per row instead of per batch, always notifying, allowing
control characters and writing another table each fail the tests named in the PR.
