# Multi-machine scheduling: state of the art and where Fabric stands
Status line: pinned to commit bc5a6d3; sources accessed 2026-11-07.

## What this component does in Fabric
- Fabric runs on one host. `Scheduler` is documented as owning one host's dispatch loop (internal/sched/sched.go:81), and the dispatch comment says fab-host "will run these concurrently" (sched.go:349). `internal/host` is the local sandbox and grant layer, not a host agent that registers with the control plane (internal/host/grant.go:1).
- There is no hosts table or registry in the migrations (0001 to 0025). `attempts.host` is a text column (0001_core.sql:85), filled from the scheduler's configured host id at START (sched.go:473, store.go:536-538) and never used for placement. `missions.host_needs` is stored (0002_missions_asks.sql:5, catalog.go:67) and not read by dispatch.
- Safety across processes already rests on the database, not on one process: attempt leases use database time (`lease_until = now() + interval`, store.go:542, 573; expired leases are found with `lease_until < now()`, store.go:609-610), writes carry a fence (`ErrStaleFence`), and a replayed START does not confer launch ownership (`TestConcurrentRunOnceHasSingleLaunchOwner`, owner_test.go:77).
- Singleton work is guarded by advisory locks only in two places: migrations (store.go:100-113) and the per-effect dispatch lock (effects/broker.go:324, 365). There is no leader election for the sweep or relay loops.

## State of the art
- Postgres as a work queue with `FOR UPDATE SKIP LOCKED`; the manual calls its view "inconsistent" and suitable for queue-like tables with many consumers. PostgreSQL docs, [SELECT](https://www.postgresql.org/docs/current/sql-select.html) (accessed 2026-11-07), H.
- River (Go): jobs in Postgres, transactional enqueue, and leader election through an unlogged table with a 5 s TTL, one leader per database and schema, which runs maintenance. [River docs](https://riverqueue.com/docs) and [leader election](https://riverqueue.com/docs/leader-election) (accessed 2026-11-07), H. The main page does not detail the claim query.
- Hatchet: Postgres is the durable store; engine and API are stateless and run as several instances; workers connect from any machine. Throughput claims are the vendor's and were not checked. [architecture](https://docs.hatchet.run/home/architecture) (accessed 2026-11-07), H.
- Nomad: leader-held evaluation broker, parallel workers, optimistic placement validated by the leader's plan queue. [scheduling concepts](https://developer.hashicorp.com/nomad/docs/concepts/scheduling/scheduling) (accessed 2026-11-07), H.
- Kubernetes Leases carry `holderIdentity`, `leaseDurationSeconds` and `renewTime` and serve node heartbeats and leader election. The page does not discuss fencing or clock skew. [Leases](https://kubernetes.io/docs/concepts/architecture/leases/) (accessed 2026-11-07), H.
- A lease alone is unsafe because a paused client can act after expiry; the protected resource should reject writes with a lower, monotonically increasing token. Kleppmann, [How to do distributed locking](https://martin.kleppmann.com/2016/02/08/how-to-do-distributed-locking.html) (2016; accessed 2026-11-07), H.
- Cluster-level quota accounting with per-queue reservation as a model for multi-host capacity. Kueue, [concepts](https://kueue.sigs.k8s.io/docs/concepts/) (accessed 2026-11-07), H.

## Comparison
| Practice | Fabric today (file:line and test) | Verdict | Why |
|---|---|---|---|
| Durable queue in Postgres, transactional state change | Missions, events and outbox in one store; commands are idempotent by (actor, mission, command id) (0001_core.sql:30-44); `TestCapRace` | parity | Same base as River and Hatchet. Fabric uses a state-machine log, not a job table. |
| Many claimers on one row set (`SKIP LOCKED`) | Not found. Dispatch reads a batch without locking and relies on START replay (sched.go:351-390) | different scope | Safe against duplicate launch per `TestConcurrentRunOnceHasSingleLaunchOwner`, but several schedulers would all scan and contend on the same 10 rows. |
| Lease plus fencing token | DB-time lease (store.go:542, 573), fence on commands; `TestAuthorizeDispatchUsesDBTimeAfterLocks` (dispatch_test.go:128), `TestStaleFenceRefused` (agent/loop_test.go:156), `TestReservationSQLBackstopsUseTriggerClock` (clock_backstop_test.go:165) | parity | This is the Kleppmann recommendation, with the clock taken from the database. Kubernetes Leases document neither. Fabric's fencing is tested per component; a fault-injection test with a paused holder across two processes was not found. |
| Host registry with capacity, heartbeat and labels | Not found | behind | Kubernetes, Nomad and Hatchet track nodes or workers. Fabric has one global `slots.cap` (store.go:313-318) with no per-host count. |
| Placement by capability or platform | `host_needs` stored, unused | behind | Nomad filters nodes by constraints. Fabric cannot yet state "needs Landlock" or "needs macOS". |
| Reclaim work from a dead host | Expired attempt leases are listed (store.go:609-610); recovery of those attempts is per attempt, not per host | parity (peer not documented) | Kubernetes and Nomad drain per node. Fabric reclaims per attempt, which covers the crash case but has no cordon or drain. |
| Leader election for singleton loops | Not found, except advisory locks listed above | behind | River elects a leader for maintenance. Running two schedulers today means both sweep. |
| Recorded sandbox attestation at placement | Boundary is computed from local control results (internal/tools/exec.go:60-70); not stored with the attempt or checked at placement | behind | Needed once hosts differ (Seatbelt versus Landlock). |
| Cross-host effect safety | Effect dispatch lock is a Postgres advisory lock on a connection (broker.go:324); readback runs from the dispatching process | different scope | Works across hosts that share the database; reachability of the remote system from each host is not modeled. |

## Upgrades, ranked
| rank | upgrade | value | effort | gate or risk addressed | ticket |
|---|---|---|---|---|---|
| 1 | State in the docs that one host is a deliberate scope limit and list rows 2 to 6 as the upgrade path | High (accuracy) | Trivial | Over-reading the "multi-host" claims in design text | TBD |
| 2 | `hosts` table with DB-time heartbeat, capacity and capabilities; effective cap becomes a sum over live hosts | High once a second host exists | Med | Single global cap, no per-host limit | TBD |
| 3 | Placement filter on `host_needs`, platform and recorded sandbox profile, with a negative test for an unfit host | Med | Low-Med | Running an attempt under a weaker sandbox | TBD |
| 4 | Cordon and drain as operator actions using the existing actor and reason rules | Med | Low | Safe host maintenance | TBD |
| 5 | Advisory-lock or TTL-row leader election for the sweep and relay loops | Med | Low-Med | Duplicate maintenance work | TBD |
| 6 | `SKIP LOCKED` claim with a property test that two claimers never take the same mission | Med | Low | Scan contention with several schedulers | TBD |

## Not verified
- Whether any recovery path reassigns a mission to a different host after a crash; only lease expiry listing was read, not the full recovery flow.
- No test was found that runs two scheduler processes against one database for more than the START race.
- Hatchet's throughput numbers were not checked; Kubernetes and River pages are silent on the points marked as gaps.
