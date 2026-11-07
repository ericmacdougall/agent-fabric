# Fencing and leases: state of the art and where Fabric stands
Status line: pinned to commit bc5a6d3; sources accessed 2026-11-07.

## What this component does in Fabric
- Starting an attempt assigns `fence = Version.AttemptFence + 1`, so fences grow per mission (`internal/reducer/reducer.go:180`). Every command naming an attempt must carry exactly that attempt's fence; smaller (stale) and larger (forged) are both `ErrStaleFence` (`reducer.go:655-660`). A refused stale command appends no event and leaves a `stale_observations` row.
- Leases use database time: `lease_until = now() + interval` on start and heartbeat (`internal/store/store.go:536-543`, `573`). A heartbeat needs the right fence and a live state (`store.go:570-596`).
- `ExpireLeases` moves expired live attempts to UNKNOWN, never to a relaunch, through a replayable command keyed by fence and observed lease time (`store.go:604-645`). The expiry rechecks `lease_until >= now()` under a row lock, so a lease renewed after the sweep read it is not expired (`store.go:396-405`).
- Effect rows are bound to a live attempt at the same fence inside the database: trigger `fabric_effects_hold_attempt` takes `FOR SHARE` on the mission and attempt rows and refuses a new intent or a move to DISPATCHED unless the attempt is live with the effect's fence (`migrations/0020_effect_resolutions.sql:155-182`); the broker checks the same in `liveFence` (`internal/effects/broker.go:253`).
- The fence is not sent to remote systems: there is no reference to it in `goodqa.go` or `publish.go`. Remote duplicate suppression rests on the idempotency key, and the repository-push executor uses a compare-and-swap push (`publish.go:37`, `--force-with-lease`).

## State of the art
- Kleppmann (2016): a lease-holder can pause (GC, page fault, a network delay of about 90 seconds in one reported incident) and write after the lease expired; checking expiry before the write does not close the window. The fix is a token that strictly increases on every grant, and a storage service that remembers the highest token seen and rejects lower ones. Locks used for correctness should sit on a consensus system or a transactional database with the token enforced on every access; Redlock lacks tokens and depends on clocks. [How to do distributed locking](https://martin.kleppmann.com/2016/02/08/how-to-do-distributed-locking.html) (accessed 2026-11-07), H.
- etcd leases detect client liveness: a TTL (described as advisory), a keepalive stream, and deletion of all attached keys on expiry or revoke. Revisions are a global logical clock, and transactions can compare `mod_revision`. The API page does not describe lock recipes or fencing. [etcd API](https://etcd.io/docs/v3.5/learning/api/) (accessed 2026-11-07), M (3.5 page only).
- Temporal retries a crashed activity and tells authors to make activities idempotent, with the key enforced by the called service; heartbeats show progress. [Temporal activities](https://docs.temporal.io/activity-definition) (accessed 2026-11-07), H. The page does not describe worker fencing.
- DBOS recovery re-invokes interrupted workflows; with several executors "some coordination is required" (Conductor or manual). The page gives no ownership or fencing detail. [DBOS architecture](https://docs.dbos.dev/architecture) (accessed 2026-11-07), H.

## Comparison
| Practice | Fabric today (file:line and test) | Verdict | Why |
|---|---|---|---|
| Token checked by the resource | Equality check in the reducer (`reducer.go:655`); effects trigger checks the fence in the same transaction as the write (`0020:155-182`); `TestStaleFence` (`store_test.go:367`), `TestFenceBelongsToMission` (`resolve_test.go:479`), `TestStaleFenceAndSubjectBusy` (`broker_test.go:174`) | parity | Kleppmann's rule is that the storage service rejects stale tokens. Here the storage service is Postgres, so check and write are atomic. Equality, not "highest seen", works because one attempt id owns one fence. |
| Check-then-write race | `FOR SHARE` hold on the attempt row; ending the attempt waits for in-flight intents; `TestDispatchedHoldsAttempt` (`resolve_test.go:242`), `TestDispatchAfterAttemptEndNoRemoteWrite` (`resolve_test.go:210`) | parity | Closes the pause-between-check-and-write window for database state. Does not apply to a remote write already in flight. |
| Tokens for external resources | Not forwarded to GoodQA or the push target; idempotency key and expected head instead (`goodqa.go:37`, `publish.go:37`); `TestPublishMovedHeadQuarantined` (`publish_test.go:132`) | behind | A zombie whose DISPATCHED row is already committed can still complete a remote write. The key makes a duplicate harmless; it does not stop a stale write that differs from the new owner's. Kleppmann requires the token at every resource. |
| Non-broker side effects of a worker | Not found: nothing fences writes a sandboxed worker makes outside the broker | behind | Only broker effects are fenced; the rest relies on sandbox containment. |
| Single time source | Lease arithmetic uses database `now()`; clock backstops tested in `TestReservationSQLBackstopsUseTriggerClock` (`clock_backstop_test.go:165`) | parity | Avoids the client wall-clock jumps Kleppmann describes. A single Postgres is a single point of failure. |
| Expiry semantics | Expired lease means UNKNOWN and a reconcile, not an automatic relaunch; `TestLeaseExpiry` (`store_test.go:407`), `TestRV22_RenewedLeaseNotExpired` (`review_test.go:123`) | different scope | etcd deletes attached keys on expiry; Temporal retries the activity. Fabric's effects can be non-idempotent and are settled by readback. |
| Consensus and HA | One Postgres; no replication handling in the repo | behind | etcd uses Raft; Kleppmann recommends a consensus system for correctness locks. Fabric is a single-host, shadow-stage system. |

No `ahead` verdicts.

## Upgrades, ranked
| rank | upgrade | value | effort | gate or risk addressed | ticket: TBD |
|---|---|---|---|---|---|
| 1 | Pass the fence in executor requests where the remote has a precondition field (push: expected head already; ticket system: a version or etag) and record a remote rejection as a stale-fence observation | Closes the in-flight zombie window for remotes that support compare-and-set | M | Stale write after lease loss | TBD |
| 2 | Kill or freeze the sandbox when `ExpireLeases` fires, with a test that the process is gone before the attempt can be relaunched | Limits unfenced non-broker writes | M | Zombie worker | TBD |
| 3 | Test with a paused executor (SIGSTOP after the DISPATCHED commit) against a remote double that records writes after expiry | Turns the in-flight window from argued to measured | S | Kleppmann pause scenario | TBD |
| 4 | Document and test Postgres failover behaviour (lease time after promotion, held FOR SHARE locks) | Names the single-database limit | M | HA | TBD |

## Not verified
- Whether the sandbox process is terminated on lease expiry: no code or test found; the expiry path changes attempt state only.
- Fence behaviour for a mission restored from backup.
- etcd behaviour newer than the 3.5 documentation page.
- That the `FOR SHARE` plus trigger ordering holds under every isolation level: tests exist for the listed cases only.
