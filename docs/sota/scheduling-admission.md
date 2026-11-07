# Scheduling and admission: state of the art and where Fabric stands
Status line: pinned to commit bc5a6d3; sources accessed 2026-11-07.

## What this component does in Fabric
- Admission is a command (`CmdAdmitMission`). The store locks the single `slots` row (`SELECT cap FROM slots WHERE id = 1 FOR UPDATE`, internal/store/store.go:313-318) and the reducer refuses with `ErrOverCapacity` when `SlotsUsed >= Cap` (internal/reducer/reducer.go:98-116). The seed value is 4 (internal/store/migrations/0001_core.sql:9).
- Dispatch selects admitted missions with no live attempt, no open wait and a passed `not_before`, `ORDER BY m.priority, m.id LIMIT 10`, and launches them one at a time (internal/sched/sched.go:351-390). `priority` is an integer, lower first (cmd/fab/main.go:191).
- A full dispatch-gate resource is backpressure: `errCapacityFull` starts no attempt, and the capacity-wait streak backs off on its own capped counter and never counts toward parking (sched.go:376-379, 392; reducer.go:281-288).
- After 6 consecutive pre-launch failures the mission is parked (`PrelaunchBudget`, internal/domain/state.go:309); backoff is 2 s doubling to 5 min (state.go:311-325). Only an operator with a reason can re-admit a parked mission (reducer.go:99-101).
- Holds on named resources (`Hold`, internal/store/reservations.go:195) are all-or-nothing per attempt; `AuthorizeDispatch` stores one grant per attempt and rechecks the lease and revision at dispatch time (internal/store/dispatch.go:31, 132-148).
- One `Scheduler` owns one host's dispatch loop (sched.go:81). The `attempts.host` column exists (0001_core.sql:85) but nothing reads it for placement.

## State of the art
- Two-stage admit: quota is reserved first, then the workload runs only when it holds a reservation and its admission checks are ready. Kueue. [Kueue concepts](https://kueue.sigs.k8s.io/docs/concepts/) (accessed 2026-11-07), H.
- Per-tenant queues feeding a shared pool, with cohorts that borrow unused quota and reclaim it by preemption. Kueue. Same page, H.
- Usage-based fair sharing with decay: lower historical usage admits first, with `usageHalfLifeTime`, a per-queue weight, and an entry penalty so one tenant cannot flood before usage updates. Kueue. [admission fair sharing](https://kueue.sigs.k8s.io/docs/concepts/admission_fair_sharing/) (accessed 2026-11-07), H.
- Queue limits with a hard cap, a guarantee, a deserved share and reclaimable overuse. Volcano. [Volcano queue](https://volcano.sh/en/docs/queue/) (accessed 2026-11-07), H. The page does not cover gang scheduling.
- Multifactor priority as a weighted sum of fair-share, age and QOS; age rises to a configurable maximum, which bounds starvation. Slurm. [priority_multifactor](https://slurm.schedmd.com/priority_multifactor.html) (accessed 2026-11-07), H.
- Optimistic parallel schedulers with a serializing plan step that can reject a plan, and at-least-once evaluation delivery. Nomad. [scheduling concepts](https://developer.hashicorp.com/nomad/docs/concepts/scheduling/scheduling) (accessed 2026-11-07), H.
- Fairness keys with weights are best-effort and degrade as the key count grows. Temporal. [priority and fairness](https://docs.temporal.io/develop/task-queue-priority-fairness) (accessed 2026-11-07), M (search summary only).
- Round-robin across concurrency keys, queueing when no slot is free, and cancel-newest or cancel-in-progress strategies. Hatchet. [concurrency](https://docs.hatchet.run/home/concurrency) (accessed 2026-11-07), H.
- Per-key concurrency with up to two constraints; ordering across keys is not guaranteed and key-queue fairness is an enterprise option. Inngest. [concurrency guide](https://www.inngest.com/docs/guides/concurrency) (accessed 2026-11-07), H.
- Bounding queued work and degrading gracefully are named mitigations for unbounded consumption. OWASP. [LLM10:2025](https://genai.owasp.org/llmrisk/llm102025-unbounded-consumption/) (accessed 2026-11-07), H.
- Circuit breakers with closed, open and half-open states, manual override, per-resource breakers and faster tripping on systemic errors. Azure Architecture Center. [circuit breaker](https://learn.microsoft.com/en-us/azure/architecture/patterns/circuit-breaker) (accessed 2026-11-07), H.

## Comparison
| Practice | Fabric today (file:line and test) | Verdict | Why |
|---|---|---|---|
| Admission cap enforced transactionally | Locked `slots` row, store.go:313-318; `TestCapRace` (store_test.go:294) admits exactly 4 of 8 concurrent; `TestCapacityIsRefused` (reducer_test.go:494) | parity | Kueue and Volcano enforce quota in a controller; Fabric's is a single global count in SQL. Equivalent guarantee, much smaller scope. |
| Reserve, then recheck at launch | Hold then grant, dispatch.go:31-148; `TestAuthorizeDispatchGrantsOnceAndReplays`, `TestDispatchGateGrantStartsExactlyOnce` (gate_test.go:60) | parity | Same two-stage shape as Kueue. The gate is optional: `TestDispatchGateOffTouchesNothing` (gate_test.go:143) shows it off by default. |
| Single launch owner when schedulers race | `TestConcurrentRunOnceHasSingleLaunchOwner` (owner_test.go:77): 4 schedulers, 1 physical start | parity | Nomad solves the same race with a plan queue. Fabric relies on the START command replay, tested only on one mission. |
| Per-tenant or per-class queues, guarantees, borrowing | One global cap; `host_needs` is stored (0002_missions_asks.sql:5) but never read by dispatch | behind | Kueue, Volcano and Hatchet have per-queue or per-key limits. Fabric has none. |
| Fair ordering with aging or usage decay | `ORDER BY priority, id` (sched.go:359); no aging, no test | behind | Slurm caps age so low priority work still runs. A steady stream of lower-numbered missions can starve higher-numbered ones. |
| Preemption | Not found | behind | Kueue and Volcano reclaim quota. Low value on one host. |
| Backpressure without failure accounting | `TestCapacityWaitNeverParks` (budget_test.go:39), `TestCapacityWaitCostsAtMostOneAttempt` (budget_test.go:134) | parity (peer not documented) | The cited peer pages do not say whether queued waits count as failures. Fabric keeps them in a separate counter. |
| Breaker with automatic recovery | Latch at 6, operator-only re-admit; `TestPrelaunchBackoffAndPark` (prelaunch_test.go:12), `TestParkedAdmitIsOperatorOnly` (park_test.go:101) | behind | The Azure pattern has a half-open probe. Fabric needs a human to re-admit, which is safe but does not self-heal. |
| Fast trip on systemic errors | All pre-launch failures count equally; native provider spend-cap errors park at once but in a different layer (see cost-control) | behind | Auth or tooling faults still use all 6 tries. |
| Queue depth limit with rejection reason | Not found: `mission new` has no depth check (cmd/fab/main.go:191-199) | behind | OWASP lists queue limits. |
| Distributed placement | Not found | different scope | Nomad and Kueue place across nodes. See multi-machine. |

## Upgrades, ranked
| rank | upgrade | value | effort | gate or risk addressed | ticket |
|---|---|---|---|---|---|
| 1 | Half-open recovery for parked missions: one probe launch after an escalating delay, operator override kept | Med-High | Low-Med | Parked work never recovers without a human | goodqa#15129 |
| 2 | Max queue depth with an explicit rejection reason and a metric | Med | Low | Unbounded queued work (OWASP LLM10) | TBD |
| 3 | Bounded aging in the dispatch order, with a test that a high-numbered mission starts under steady low-numbered load | Med | Low | Starvation | TBD |
| 4 | Classify clearly systemic pre-launch errors and park them at once | Med | Low | Wasted retries, slow signal | TBD |
| 5 | Per-class slot guarantee plus the global cap | Med | Med | One class filling all slots | TBD |
| 6 | Per-repo or per-tenant key limit with round-robin | Med only if multi-tenant | Med | Fairness across tenants | TBD |
| 7 | Preemption of lower-priority attempts | Low on one host | High | Latency for urgent work | TBD |

## Not verified
- Starvation under sustained load was reasoned from the SQL order, not reproduced. Deferred missions leave the batch through `not_before` (store.go:483), so head-of-line blocking by the LIMIT 10 window was not found, but no test covers it.
- Whether `host_needs` is meant to be a placement label or a display field: only catalog.go and the CLI use it.
- Temporal fairness details come from a search summary, not the page.
- The research note proposed an accelerated trip as missing in general; the provider layer does park some errors at once (client.go:198-202), so the gap is specific to scheduler pre-launch failures.
