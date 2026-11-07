# Durable state and events: state of the art and where Fabric stands
Status line: pinned to commit bc5a6d3; sources accessed 2026-11-07.

## What this component does in Fabric
- Every mission has an append-only event log with a gap-free per-mission sequence; the mission row lock serialises writers (`internal/store/migrations/0001_core.sql:47-57`, primary key `(mission_id, seq)`).
- State is a pure fold of events: `Decide` produces events from a command (`internal/reducer/reducer.go:38`), `Apply` folds one event (`reducer.go:666`), `Replay` rebuilds a mission and stops at the first unknown kind (`reducer.go:834`).
- `Store.Execute` runs a command under its idempotency key (actor, mission, command id). Same key and same request hash returns the stored response; a different hash is `ErrIdempotencyConflict` (`store.go:261`). Refusals are stored like successes.
- `events` and `commands` reject UPDATE, DELETE and TRUNCATE by trigger (`migrations/0011_command_bytes.sql:38-44`).
- Each event is also written to an `outbox` in the same transaction (`store.go:456-461`); consumers read pages by cursor, and only rows whose transaction id is below `pg_snapshot_xmin` are visible, so a late commit is not skipped (`store.go:745-770`).
- A build refuses to append to a log holding an event kind it does not know (`store.go:365-371`).

## State of the art
- Temporal keeps an Event History as "an append-only log of Events", persisted by the service so a workflow recovers after a crash. It warns at 10,240 events and terminates the execution above 51,200 events, 2,000 updates or 10,000 signals; Continue-As-New starts a fresh execution to stay under the limits. [Temporal Event History](https://docs.temporal.io/workflow-execution/event) (accessed 2026-11-07), H.
- Temporal recovers by re-running workflow code from the start against the history; the code "has to make the same decisions when given the same history", and external calls go in Activities whose recorded results are not re-executed on replay. [Temporal Workflows](https://docs.temporal.io/workflows) (accessed 2026-11-07), H.
- DBOS stores workflow inputs and step outputs in a Postgres system database; on recovery it re-invokes interrupted workflows and skips steps whose output is checkpointed. Workflows must be deterministic, and DBOS versions workflows so they recover only on compatible code. [DBOS architecture](https://docs.dbos.dev/architecture) (accessed 2026-11-07), H. The page makes no exactly-once claim for transactions.
- Transactional outbox: write the message to an outbox table in the same transaction as the business change; a relay (log tailing or polling publisher) forwards it. The relay "might publish a message more than once", so consumers must be idempotent, and ordering must be preserved per aggregate. [microservices.io outbox](https://microservices.io/patterns/data/transactional-outbox.html) (accessed 2026-11-07), H.
- Stripe API v1 saves the status code and body of the first request under an idempotency key, including 500s, compares parameters on retry and errors on mismatch, and may prune keys after 24 hours. [Stripe idempotent requests](https://docs.stripe.com/api/idempotent_requests) (accessed 2026-11-07), H.
- AWS: record the client token atomically with the mutation, return a semantically equivalent response on retry, return a validation error for the same token with different parameters, and expire tokens after a bounded period. [AWS Builders Library](https://aws.amazon.com/builders-library/making-retries-safe-with-idempotent-APIs/) (accessed 2026-11-07), H.

## Comparison
| Practice | Fabric today (file:line and test) | Verdict | Why |
|---|---|---|---|
| Append-only, ordered history | Gap-free `(mission_id, seq)` key, `0001_core.sql:47-57`; update/delete/truncate triggers, `0011:38-44`; `TestEventSeqGapFreeAndRebuild` (`store_test.go:456`) | parity | Temporal's history is also append-only. Triggers are bypassable by the table owner and there is no hash chain (see observability notes). |
| Recover state by replay | Pure reducer fold, `reducer.go:834`; seeded `TestTraces` checks replay equals live state (`reducer_test.go:343`) | different scope | Temporal and DBOS replay user workflow code and need determinism rules. Fabric replays only a reducer; the agent loop is not replayed, so a lost turn is not resumed from history. |
| Command idempotency | Stored response per `(actor, mission, id)`, hash mismatch is a conflict, `store.go:261`; `TestCommandReplay` (`store_test.go:178`); `TestReplayBytesExact` (`replay_bytes_test.go:15`) | parity | Same shape as Stripe and AWS. Fabric stores refusals too and never prunes receipts (append-only trigger); Stripe v1 prunes after 24 hours. Unbounded growth is untested. |
| Outbox without skipped rows | Id order below `pg_snapshot_xmin`, cursor never moves back (`store.go:745-779`); `TestRV3_OutboxDeliversLateCommit` (`review_test.go:19`), `TestOutboxPagesAndCursor` (`store_test.go:512`) | parity (peer not documented) | The microservices.io page does not discuss late-committing transactions in a polling relay. Delivery is at-least-once; consumer idempotency is the consumer's job. |
| Unknown or future event kinds | Replay and `Execute` refuse (`reducer.go:834`, `store.go:365-371`); `TestReplayStopsAtFirstUnknownEvent` (`replay_test.go:35`), `TestVerifiedSnapshotRejectsUnknownEvent` (`rebuild_test.go:74`) | behind | Failing closed is safe, but there is no upcasting or version routing. DBOS recovers a workflow only on compatible code; Temporal has versioning APIs. In Fabric a rolling upgrade or rollback parks every mission with a newer kind. |
| Bounded history | Not found: no archival, checkpoint-and-truncate or per-mission size alarm | behind | Temporal enforces limits and offers Continue-As-New. Fabric has a mission snapshot row (`store.go:323`) but I found no limit on log length. |
| Verified rebuild | `reducer.Digest` (`check.go:276`); `TestVerifiedSnapshotIsReadOnlyAndDetectsCorruption` (`rebuild_test.go:48`) | parity (peer not documented) | Neither Temporal nor DBOS pages describe comparing a stored snapshot to a replay. |

No `ahead` verdicts: each strength Fabric has is also present in at least one peer's documentation or the peer pages are silent.

## Upgrades, ranked
| rank | upgrade | value | effort | gate or risk addressed | ticket: TBD |
|---|---|---|---|---|---|
| 1 | Event schema version field, upcasters for old kinds, and a minimum-reader-version recorded in the database so an older binary refuses to start rather than refusing per command | Safe rolling upgrade and rollback | M | Mixed-version writes; parked missions after rollback | TBD |
| 2 | Per-mission event count and byte alarms, plus a checkpoint event with `Digest` that replay can start from | Bounded replay cost, early warning | M | Unbounded log growth | TBD |
| 3 | Receipt retention policy for `commands` (archive after N days, keep the hash) with a test that a replay after archive still conflicts on a changed body | Bounded storage without losing conflict detection | M | Idempotency table growth | TBD |
| 4 | Hash chain over events written by a role that cannot disable triggers | Tamper evidence | M | Audit integrity | TBD |
| 5 | Outbox lag metric per consumer and an alarm | Detects a stuck consumer | S | Silent projection staleness | TBD |

## Not verified
- Whether any production-sized mission log was measured; no limit on log length was found, which is not proof none exists.
- Backward behaviour when a newer binary reads an older log (no test found; the code reads old kinds because the registry only grows).
- Temporal's 50 MB history size figure: the fetched page did not state it, so it is not quoted.
- DBOS idempotency-key and executor-ownership behaviour: the architecture page does not cover them.
