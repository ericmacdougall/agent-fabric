# Review gates

A review gate is a point where Fabric, or the process that changes Fabric, refuses to
proceed without proof. This document lists every gate, says what it protects against, says
exactly what it enforces and where, and says how a reader can check that it still works.

Everything here is pinned to commit `bc5a6d3` (`file:line` references are at that commit;
lines drift, the function names do not). A claim of a guarantee always names the code that
enforces it and a test that fails when the code is broken. Where no such test exists the
claim is moved to [Known gaps](#known-gaps) or marked "not yet tested". Marketing words are
avoided on purpose: whether the design is state of the art is argued in
[Comparison and rationale](#comparison-and-rationale), not asserted.

Scope note: this describes `main` at `bc5a6d3`. Work in flight on the provider path (goodqa#15087: migration `0026`, provider and attempt-path guards) is not described as shipped; where it bears on a gate it is called out as in flight.

Related documents: [threat model](threat-model.md), [gate angles we do not yet
cover](gate-angles.md), [state-of-the-art notes per component](sota/README.md).

## Contents

1. [How to read a gate](#how-to-read-a-gate)
2. [Gate map](#gate-map)
3. [Runtime gates](#runtime-gates) (R1 to R25)
4. [Development and change gates](#development-and-change-gates) (D1 to D8)
5. [Evidence: failing controls and test results](#evidence-failing-controls-and-test-results)
6. [Comparison and rationale](#comparison-and-rationale)
7. [Known gaps](#known-gaps)
8. [Gate angles we do not yet cover](#gate-angles-we-do-not-yet-cover)
9. [Reproducing the checks](#reproducing-the-checks)

## How to read a gate

Each gate has the same fields.

| Field | Meaning |
|---|---|
| Threat | What goes wrong without the gate. |
| Invariant | The exact condition the gate enforces, in one sentence. |
| Enforced at | `file:line` at `bc5a6d3`. |
| Why deterministic | The mechanism: a DB constraint, a trigger, a fencing token, a pure state machine, a lock. A gate that rests on a model's judgement says so. |
| Fails if broken | The test that must fail when the gate is removed, and, when one exists, the must-fail mutant from `tools/mutants/mutants.json` that removes it. |
| Limits | What the gate does not cover. |

**Strength labels.** Every gate carries one:

- **DB-enforced**: a constraint, trigger, privilege or lock in Postgres holds even if the Go code is wrong or bypassed by a role that can reach the table.
- **Code-enforced**: a pure function or a transaction in Go. Holds for any caller that goes through it; a caller with direct SQL access to the same role is not stopped.
- **Convention**: a rule that depends on a person or an external tool following it.

**Test commands.** All database tests need a disposable PostgreSQL 16 and run one test as:

```
FABRIC_TEST_DSN='postgres://USER:PASS@127.0.0.1:PORT/postgres?sslmode=disable' \
  go test -count=1 -v -run '^TestName$' ./internal/<pkg>
```

The native-loop tests also need `FABRIC_HOOKS_DIR` to point at the harness hook scripts the
policy oracle calls. Pure packages (`reducer`, `domain`, `host`, `policy`, `tools`) need no
database. A test creates its own database inside the server, so the DSN names a server, not a
database you care about.

**Evidence level of this document.** Section [Evidence](#evidence-failing-controls-and-test-results)
records what was actually run for this review: the full `./internal/... ./cmd/...` suite at
`bc5a6d3` (every cited test below was observed to print `--- PASS`, except the four failures
listed there), a selection of the repository's own must-fail mutants, and nine extra
throwaway failing controls written for this review.

## Gate map

```mermaid
flowchart TD
  subgraph Operator["Operator and attribution"]
    OP["FABRIC_ACTOR=op:name + --reason (R20)"]
  end
  subgraph Admission["Admission and launch"]
    A1["Slot cap on slots row lock (R1)"]
    A2["Dispatch gate: HELD reservations + grant (R2)"]
    A3["Final admission: fence, RUNNING, lease, intent revision (R3)"]
    A4["Park after 6 prelaunch failures (R8)"]
  end
  subgraph Attempt["Attempt authority"]
    F1["Fence check on every command (R4)"]
    F2["DB-time lease + heartbeat + expiry (R5)"]
    F3["Settle only on exit 0 + coherent stream (R6)"]
    F4["Settle retry, UNKNOWN reconcile, sweeps (R7, R9, R11)"]
  end
  subgraph Effects["Effect broker"]
    E1["INTENT: key + request hash (R14a)"]
    E2["Approval bound to hash (R14b)"]
    E3["One live effect per subject (R14c)"]
    E4["DISPATCHED: advisory lock + deadline (R14h)"]
    E5["UNKNOWN: readback only, never retry (R14d, R14e)"]
    E6["QUARANTINED: operator + audit row (R14f)"]
    E7["Outcome guard trigger, every role (R14g)"]
  end
  subgraph Review["Review and done"]
    V1["Evidence arbitration (R12)"]
    V2["Accept needs VERIFYING + VERIFIED (R12)"]
    V3["Done needs live test of deployed commit (R13)"]
  end
  subgraph Containment["Containment"]
    C1["Native spend caps: run, job, month (R15)"]
    C2["Credential and key-shape checks (R16)"]
    C3["Host sandbox profiles (R17)"]
    C4["DB roles: app vs runner, 0025 (R18, R19)"]
    C5["Native tool confinement + oracle (R23)"]
  end
  OP --> A4
  OP --> E6
  A1 --> A2 --> A3 --> F1
  A4 -. blocks .-> A1
  F1 --> F2 --> F3 --> V1 --> V2 --> V3
  F1 --> E1 --> E2 --> E3 --> E4 --> E5 --> E6
  E7 -. guards all .-> E1
  C1 -. bounds .-> F3
  C3 -. confines .-> F1
  C4 -. limits writes of .-> E1
  C5 -. gates tool calls of .-> C1
```

## Runtime gates

### R1. Admission and the slot cap

- **Threat.** Unbounded concurrent missions exhaust the host, the budget and the review capacity.
- **Invariant.** A mission may enter an executing state only if fewer than `cap` other missions hold a slot (`ADMITTED`, `RUNNING`, `WAITING` or `PAUSED`).
- **Enforced at.** `internal/store/store.go:313-318` locks the single `slots` row `FOR UPDATE` before counting; `store.go:420-422` counts the slot holders in the same transaction; `internal/reducer/reducer.go:98-116` (`CmdAdmitMission`) refuses with `ErrOverCapacity` when `SlotsUsed >= Cap`. A PAUSED or parked mission keeps its slot, so a stuck mission cannot be bypassed by starting another.
- **Why deterministic.** The cap is a transaction (a row lock plus a count), and the decision is a pure function of the mission log and `Env{Cap, SlotsUsed}`.
- **Fails if broken.** `TestCapacityIsRefused` (`./internal/reducer`, mutant `M3_capacity_ignored`); `TestCapRace` (`./internal/store`, mutant `M12_cap_lock_removed`).
- **Limits.** The cap counts missions, not CPU, memory or money; those are R2 and R15. `Store.SetSlotCap` (`store.go:198`) is a plain `UPDATE` available to the app role with no reason or audit row (see [Known gaps](#known-gaps), G-12).

### R2. Dispatch gate (resource reservations and grant)

- **Threat.** The scheduler over-admits a resource (a heavy host, a paid-model budget) because two daemons or two ticks race.
- **Invariant.** With `fab daemon --hold` configured, a runner starts only after a stored grant that names every required resource, taken under row locks, against database time, for the attempt's own fence.
- **Enforced at.** `internal/store/dispatch.go:31` (`AuthorizeDispatch`): locks the attempt row, then its HELD reservations in id order, reads `clock_timestamp()` after the locks (`:88`), checks fence, state, lease, intent revision and unexpired reservations, and stores exactly one decision in `dispatch_grants` (`0012_dispatch_grants.sql`), which later callers replay (`Replayed`) instead of re-deciding. The scheduler calls it at `internal/sched/sched.go:553` and abandons the attempt on denial.
- **Why deterministic.** A unique primary key on `dispatch_grants.attempt_id`, locks taken in sorted order (no deadlock), and the database clock instead of process clocks.
- **Fails if broken.** `TestAuthorizeDispatchGrantsOnceAndReplays`, `TestAuthorizeDispatchDenials` (mutants `M126` to `M131`), `TestAuthorizeDispatchUsesDBTimeAfterLocks` (`M125`), `TestHoldCapacityRace` (mutant `M45` currently times out, see Evidence), `TestDispatchGateGrantStartsExactlyOnce`, `TestDispatchGateDenialsNeverStart` (`M133`) in `./internal/store` and `./internal/sched`.
- **Limits.** Off by default. The README states plainly that the gate "is an accident guard, not a security boundary yet": a runner that holds the runner role can still write some rows directly (R18). Capacity is a number an operator sets (`fab db capacity`), not measured from the host.

### R3. Final admission (goodqa#15082 T7)

- **Threat.** An attempt was admitted, then its lease lapsed, it was superseded, or its intent was revised while the launch was being prepared; the runner would still start on stale authority.
- **Invariant.** Immediately before the runner starts, in every deployment (gated or not), the attempt must still be at its fence, `RUNNING` (after the durable `ACK_START`), unexpired by database time, and at the mission's current intent revision. A database error at this point abandons the attempt before launch.
- **Enforced at.** `internal/store/dispatch.go:132-146` (`staleReason`, shared by both paths); `:148` `CheckDispatchFresh` for the ungated path; `internal/sched/sched.go:535-560` calls it after `ACK_START` commits and before any runner byte (`s.ln.Start`, `:569`). The `ACK_START` is committed first (`:526`) so a crash after it leaves a leased attempt that expires into `UNKNOWN`, never a silent second launch.
- **Why deterministic.** One SQL read against `clock_timestamp()` joined to the mission's `intent_revision`; the verdict is a pure function of the row values (`staleReason`).
- **Fails if broken.** `TestUngatedFinalAdmission` (`./internal/sched`). Failing control C1 (below): replacing the lease clause with `case false:` makes it fail. For the gated path, `TestAuthorizeDispatchDenials` with mutant `M127_grant_lease_unchecked`.
- **Limits.**
  - The check and `exec` are not one atomic step. A lease of 5 minutes (`Store.Lease`, `store.go:78`) and the heartbeat bound the window, but a worker can start with authority that expires a few milliseconds later. The fence still stops it from writing mission state afterwards (R4), but it can already have started.
  - The ungated path stores no decision row, so there is no audit trail of the check.
  - A database outage at the check parks missions after six tries (R8); re-admission is manual by design.

### R4. Attempt fencing

- **Threat.** A superseded or crashed worker (a "zombie") keeps acting after a new attempt took over and corrupts state.
- **Invariant.** Every command that touches an attempt carries the attempt's fence, and is refused unless it equals the attempt's own fence. A smaller fence is a stale message; a larger one is forged. A refusal under a stale fence is recorded in `stale_observations`.
- **Enforced at.** `internal/reducer/reducer.go:655` (`fenceCheck`), called from `liveAttempt` (`:607`) for ack, evidence, finish, expire, submit-effect and reconcile; `internal/store/store.go:441` records the observation; `Store.Heartbeat` (`store.go:572`) includes `fence = $2` in the `WHERE`; the broker re-checks the fence in `liveFence` (`internal/effects/broker.go:253`, called twice around the precondition, `:456`) and the migration `0020` trigger `fabric_effects_hold_attempt` refuses a new intent or a move to `DISPATCHED` unless the attempt is live at that fence, holding the attempt row `FOR SHARE`.
- **Why deterministic.** The fence is a monotone integer assigned by the reducer (`AttemptFence + 1` at `CmdStartAttempt`, `reducer.go:153-180`); equality is the only test. The trigger repeats it in the database for every role, so it holds against Go-side mistakes in the broker.
- **Fails if broken.** `TestStaleFenceRefused` (`./internal/reducer`, mutant `M1_stale_fence_overwrite`), `TestStaleFence` (`./internal/store`, `M14`), `TestStaleFenceAndSubjectBusy`, `TestFenceBelongsToMission`, `TestDispatchAfterAttemptEndNoRemoteWrite` (`./internal/effects`).
- **Limits.**
  - A fence protects Fabric's own state and the broker. It does not stop a zombie process from doing things outside Fabric (writing to the filesystem, calling a network service) before the sandbox stop reaps it (R17). The effect broker passes the idempotency key, not the fence, to the remote system, so a remote cannot itself reject a stale fence. This is the standard fencing-token caveat (see [fencing sota](sota/fencing-leases.md)).
  - The runner role is shared by every runner on a host (R18): it can act for any running mission's fence if it learns it.

### R5. Leases, heartbeats and expiry

- **Threat.** A runner dies or hangs and holds a slot and a subject forever; or a live runner is declared dead and a second one starts beside it.
- **Invariant.** An attempt in `LEASED`, `PREPARING` or `RUNNING` has a `lease_until` set by database time. A heartbeat extends it only for the matching fence and a live state. An attempt whose lease ran out is moved to `UNKNOWN` (not `FAILED`), keeps its slot, its reservations and its subject, and blocks a new attempt until reconciled. A lease renewed after the sweep read its page is not expired.
- **Enforced at.** `internal/store/store.go:572-596` (`Heartbeat`), `:608` (`ExpireLeases`), `:396-405` (authority rechecked under `FOR UPDATE` before the mutation); `internal/sched/sched.go:671` (the heartbeat goroutine, which keeps beating while a settle and its retries run, `:614`).
- **Why deterministic.** `now()` and `clock_timestamp()` in the database, not worker clocks. The expiry decision is taken inside the same transaction as the row lock.
- **Fails if broken.** `TestLeaseExpiry` and `TestRV22_RenewedLeaseNotExpired` (`./internal/store`, mutant `M35_lease_recheck_removed`), `TestRenewedLeaseRefusalReplays`, `TestOwnerHeartbeatsLiveRun`, `TestHeartbeatSurvivesTransientError` (`./internal/sched`).
- **Limits.** One lease TTL (5 minutes) serves as both a liveness bound and a hard deadline; there is no separate progress heartbeat. No test moves the database clock backwards (see [Known gaps](#known-gaps), G-14).

### R6. Settlement rule: a run is verified only on a clean exit and a coherent stream (T1)

- **Threat.** A runner emits a "success" event, then crashes, exits non-zero, or contradicts itself, and the mission advances on the strength of the last event.
- **Invariant.** A run ends as a checkpoint only if the process exit code is 0, the stream parses, and the stream holds no contradiction; otherwise the attempt is abandoned with the reason. If waiting for the process fails, nothing is settled and the lease is left to expire into `UNKNOWN`.
- **Enforced at.** `internal/sched/sched.go:810-835` (exit code and `run.Contradictions`, then a checkpoint with a recognised end kind); `:613-624` (`afterWait` with a Wait error); the stream parser `internal/provider/claude/stream.go` records contradictions (a later success does not hide an earlier failure).
- **Why deterministic.** Pure functions of the exit code and a bounded parse of the recorded `stream.jsonl`.
- **Fails if broken.** `TestSettleNeedsCleanExitAndCoherentStream` (`./internal/sched`; mutants `M346` to `M348`), `TestParseContradictions` (`./internal/provider/claude`, `M349`).
- **Limits.** "Verified" here means the run ended coherently, not that its claims are true. Whether the claimed work is real is R12, and R12 has its own limit.

### R7. Settlement retry (T6)

- **Threat.** One transient database error while recording an observed exit drops the exit, leaves the attempt to expire into `UNKNOWN`, and turns a good run into a manual reconcile.
- **Invariant.** After an observed exit, the finish command is decided once from the stream and its store calls (idempotent `finish:` and `verify:` command ids) are retried with capped backoff while the heartbeat keeps the lease alive.
- **Enforced at.** `internal/sched/sched.go:625-660` (`settleTries`, backoff capped at a third of the lease).
- **Why deterministic.** Idempotent command ids make a retry replay the stored response rather than apply twice (R21).
- **Fails if broken.** `TestSettleSurvivesOneTransientFailure` (`./internal/sched`). Failing control C8 (below).
- **Limits.** The number of tries is finite; after it the lease expires into `UNKNOWN` and a reconcile (R11) decides. Only one injected failure class (a trigger raising once) is tested.

### R8. Park after repeated pre-launch failure (T2)

- **Threat.** A mission whose launch always fails (missing worktree, refused grant, database outage) loops forever, burning slots and noise.
- **Invariant.** After `PrelaunchBudget = 6` (`internal/domain/state.go:309`) consecutive attempts abandoned before launch, the mission is parked: no attempt starts, and once nothing is live it becomes `BLOCKED` and frees its slot. Only an operator (actor `op:<name>`, with a reason) clears a park. A park recorded while another attempt was live is kept, and re-applied when that attempt ends (`withRepark`). Capacity waits are backpressure and never count toward the park.
- **Enforced at.** `internal/reducer/reducer.go:98-110` (readmit needs `env.Operator` and a reason), `:71-85` (`withRepark`), the abandon branch with `cmd.Prelaunch` (`:268-295`); `internal/store/store.go:409` derives `Operator` from the actor string.
- **Why deterministic.** The park is an event in the mission log; the rules are in the pure reducer; replay of an old log without the fix still replays (`TestOldParkLogReplaysAndCanBeReadmitted`).
- **Fails if broken.** `TestParkHeldWhileAnotherAttemptLive`, `TestParkedAdmitIsOperatorOnly`, `TestPrelaunchBackoffAndPark` (mutant `M137_prelaunch_never_parks`), `TestLaunchedFinishResetsPrelaunchCount` (`./internal/reducer`); `TestParkedAdmitRefusedForAutomationActors`, `TestReparkUnderAppRole` (`./internal/store`). Failing controls C3 and C6 (below).
- **Limits.** The operator check is on a self-asserted string (R20). A database outage at final admission counts toward the park (fail closed, but needs manual re-admission afterwards).

### R9. Sweep isolation (T3)

- **Threat.** One poisoned row (a reservation whose close fails) blocks every row after it, so expired holds are never released and capacity leaks.
- **Invariant.** Sweeps continue past a row that fails; each failure is reported with its row id; the dispatch loop parks only the mission that errored.
- **Enforced at.** `internal/store/reservations.go:446-462` (collect errors, `continue`); `internal/sched/sched.go:376-388` (per-mission park).
- **Why deterministic.** A loop with per-row error collection (`errors.Join`); no shared state between rows.
- **Fails if broken.** `TestSweepsIsolatePerRowFailures` (`./internal/store`, injects a failing trigger on one row), `TestPerMissionErrorDoesNotStopTheLoop`, `TestDispatchErrorParksOnlyThatMission` (`./internal/sched`). Failing control C5 (below).
- **Limits.** The tests inject database-side failures; a panic in a sweep is not covered.

### R10. Unpausing a paused mission

- **Threat.** A mission paused because an attempt was quarantined resumes while the quarantined runner may still be alive, or while an unsettled effect has an unknown outcome.
- **Invariant.** Only an operator, with a reason, and only when no attempt is live or unresolved and no broker effect is unsettled, returns a PAUSED mission to ADMITTED; the mission keeps its slot; a parked mission re-parks.
- **Enforced at.** `internal/reducer/reducer.go:117-143`; unsettled effects counted in `internal/store/store.go:413-416`.
- **Why deterministic.** Pure reducer plus a count taken in the same transaction.
- **Fails if broken.** `TestUnpauseIsOperatorOnly` (`./internal/reducer`), `TestUnpauseRefusedWithUnsettledEffects`, `TestUnpauseOfParkedMissionFreesTheSlot` (`./internal/reducer`), `TestUnpauseCountsUnsettledEffects` (`./internal/store`). Failing control C4 (below).
- **Limits.** The operator attests that the runner is gone. "Nothing checks by machine that its runner is gone" (README). A mistaken attestation can start a second worker beside a live one.

### R11. UNKNOWN attempts are reconciled, never retried blindly

- **Threat.** An attempt whose outcome is unknown is restarted, so work and effects run twice.
- **Invariant.** A mission with an unresolved attempt cannot start another (`ErrUnknownUnresolved`), cannot release its slot (`CmdRelease`), and cannot complete. Reconciliation reads the runner back by attempt id; a runner that is provably gone is quarantined, one that is alive keeps its slot.
- **Enforced at.** `internal/reducer/reducer.go:166` (start refuses), `:531-539` (release refuses), `:589` (done refuses); `internal/sched/sched.go:714` (`reconcileUnknown`).
- **Why deterministic.** State-machine guards in the reducer; the prober answers known/unknown explicitly, and "no answer" keeps `UNKNOWN`.
- **Fails if broken.** `TestRV5_ReleaseRefusedWhileUnknown`, `TestUnknownRetryBlocked` (mutant `M4`), `TestRV4_NoResubmitWhileTwinUnresolved` (`./internal/reducer`); `TestRestartReconcilesGoneRunner`, `TestRestartKeepsLiveRunnerSlot`, `TestRestartWithoutReadbackKeepsUnknown` (`./internal/sched`).
- **Limits.** Only as good as the prober; a process-inventory error on a host with the Linux containment gaps (R17) can misjudge.

### R12. Evidence arbitration and acceptance (the review gate)

- **Threat.** A worker declares "ready for review" with claims that are not backed by evidence for the current candidate, or an old receipt is reused for a new head.
- **Invariant.**
  - A `ready_for_review` claim is a claim, not a fact: every claimed acceptance criterion must cite a receipt that this attempt recorded for the exact current (intent revision, candidate generation, head), and the receipt must not be vacuous (nothing passed and nothing failed). A failing claim is downgraded to a checkpoint and recorded as `CLAIM_REFUSED`.
  - `ACCEPT` needs the candidate in `VERIFYING`, the attempt `VERIFIED`, and current evidence. Revising the intent revokes acceptance. `DELIVER` needs the accepted revision, generation and head to match the current ones.
- **Enforced at.** `internal/reducer/reducer.go:626-648` (`arbitrate`), `:237-253` (the `ready_for_review` branch), `:352-379` (`CmdAccept`), `:380-396` (`CmdDeliver`); evidence keys include attempt, revision, generation, head and receipt (`domain.EvidenceKey`).
- **Why deterministic.** A pure function of the recorded event log. Replay produces the same verdict (`TestReplay*` in `replay_test.go`).
- **Fails if broken.** `TestReadyForReviewArbitration` (mutants `M17`, `M19`), `TestRV6_AcceptOnlyAfterReview` (`M33`), `TestRV7_FutureSubjectEvidenceRefused`, `TestRV17_EvidenceKeyNoCollision` (`./internal/reducer`).
- **Limits (important).** The receipt is a string the worker recorded; `vacuous` is a flag the recording caller sets. The gate proves the claim *refers to* evidence for the current head. It does not prove the evidence is true, and there is no independent verifier, second model or quorum. That is the largest limit in the review path ([gate angles](gate-angles.md): independent verifier, quorum). Fabric does not run tests itself.

### R13. "Done" means live-tested (merged is not done)

- **Threat.** A mission is declared complete because it merged or deployed, while the deployed build was never exercised.
- **Invariant.** `COMPLETE_MISSION` requires delivery `LIVE_VERIFIED` by a non-vacuous live-test receipt that names a host and a live target for the deployed commit of the current generation; no attempt live or unresolved; no broker effect unsettled (the effects table counts, read in the same transaction).
- **Enforced at.** `internal/reducer/reducer.go:543-570` (`CmdRecordLiveTest`), `:571-604` (`CmdCompleteMission`); `internal/store/store.go:411-417` supplies `UnsettledEffects` in the command's transaction; `0020` makes an in-flight effect hold its attempt row so none can appear after the count.
- **Why deterministic.** Pure reducer; the count shares the transaction.
- **Fails if broken.** `TestDoneNeedsLiveTestForDeployedCommit` (mutant `M203`), `TestDoneRefusedWhileAttemptLive` (`M205`), `TestDoneRefusedWhileEffectUnsettled` (`M208`), `TestRevisedIntentClearsLiveTest` (`M209`) in `./internal/reducer`; `TestDoneCountsBrokerEffects` (`M210`, `M211`, `M225`), `TestDoneNeedsLiveTestThroughStore` in `./internal/store`.
- **Limits.** The live-test receipt is recorded by the caller of `RecordLiveTest`. The caller is not authenticated beyond the actor string, and Fabric does not perform or verify the live test (R20). Independent click-testing is a process gate (D5), not code.

### R14. The effect broker

An effect is an action in the outside world (a ticket comment, a git push). The broker is a
state machine: `INTENT` (or `APPROVAL_REQUIRED` then `APPROVED`) to `DISPATCHED`, then
`CONFIRMED`, `FAILED_DEFINITE` or `UNKNOWN`; `UNKNOWN` to `RECONCILING` to `CONFIRMED`,
`NOT_APPLIED`, `QUARANTINED` or back to `UNKNOWN`; `QUARANTINED` to `RESOLVED_APPLIED` or
`RESOLVED_NOT_APPLIED` by an operator only. Its sub-gates:

**R14a. Identity: key plus canonical request hash.**
Threat: a retry sends a different body under the same key, or two identical requests collapse.
Invariant: one idempotency key binds the whole intent (mission, attempt, fence, kind, subject, request hash, needs-approval); the same key with anything different is `IDEMPOTENCY_CONFLICT`; the request is canonical JSON (no exponents) and its hash is recomputed before transport.
Enforced at `internal/effects/broker.go:196-250` (`Submit`), `:409-412` (recompute, `ErrRequestTampered`); `internal/domain/canonical.go`; `effects.idem_key UNIQUE` (`0003_effects.sql`).
Tests: `TestSameKeyDifferentBodyConflicts`, `TestReplayBindsWholeIntent` (mutants `M118`, `M119`, `M149`), `TestDispatchRefusesRequestThatNoLongerMatchesItsHash` (`M120`), `TestNonCanonicalNumberRefusedAtIntake`, `TestCommandHashIgnoresKeyOrderWithExponent` (`./internal/domain`).
Strength: DB-enforced for the key (unique index) and column immutability (R14g); code-enforced for the hash recompute.

**R14b. Approval bound to the request hash.**
Threat: a request is approved, then altered, then sent.
Invariant: `Approve` succeeds only if the cited hash equals the stored request hash and the state is `APPROVAL_REQUIRED`; an effect that carries an approval dispatches only as `APPROVED`; the trigger lets `approval_sha`/`approved_by` change once, to the request hash, with a non-empty approver.
Enforced at `broker.go:283-300` (`Approve`, the `WHERE ... request_sha = $3`), `:413-427` (dispatch refuses `INTENT` with an approval, and `APPROVED` with a mismatching hash), `0020_effect_resolutions.sql` (approval changes only by approve).
Tests: `TestApprovalBindsRequestHash` (mutant `M27`), `TestDispatchRefusesUnapprovedIntentWithApproval` (`M247`, `M249`), `TestRunnerCannotApproveEffects` (`M186`; M184 currently survives, see Evidence), `TestEffectColumnsImmutable`.
Limits: see [Known gaps](#known-gaps) G-1 and G-2. Approval is optional per submission except for kind `publish` (`publish.go:125`), and `approved_by` is an unauthenticated string. `fab effect comment --approve` approves its own submission in the same process (`cmd/fab/effect.go:148-155`): the control is hash binding, not separation of duties.

**R14c. One live effect per subject.**
Threat: two concurrent writes to the same remote object.
Invariant: at most one effect per `subject` in `DISPATCHED`, `UNKNOWN` or `RECONCILING`.
Enforced at `0003_effects.sql:26` (partial unique index `effects_live_subject_idx`); `broker.go` maps the violation to `ErrSubjectBusy`.
Tests: `TestStaleFenceAndSubjectBusy` (mutant `M28`). Strength: DB-enforced.

**R14d. An unknown outcome is never retried.**
Threat: a lost response is retried and the remote applies the effect twice.
Invariant: a transport error or ambiguous result settles `UNKNOWN`; `Retry` is refused for `UNKNOWN`; `UNKNOWN` is never `FAILED_DEFINITE`; only a readback may say `NOT_APPLIED`; a submit of the same request is refused while the twin is `UNKNOWN`.
Enforced at `broker.go:305` (`Dispatch`), `:664` (`Retry`), `internal/reducer/reducer.go:422-445`, `:492-535`.
Tests: `TestPostCommitLossReconcilesToConfirmed` (mutant `M26`), `TestPreCommitLossRetriesSameKey`, `TestUnknownCannotFailDefiniteWithoutReadback` (`M122`), `TestNotAppliedNeedsUnknownFirst` (`M123`), `TestLateResultForEarlierSubmissionIsRefused` (`M124`), `TestRV4_NoResubmitWhileTwinUnresolved` (`M31`).

**R14e. Readback reconciliation.**
Threat: after a lost response nobody knows whether the remote applied the effect.
Invariant: `Reconcile` is a token-free readback that never dispatches: committed to `CONFIRMED`, absent to `NOT_APPLIED`, conflicting (e.g. the remote head moved) to `QUARANTINED`, unreadable stays `UNKNOWN`.
Enforced at `broker.go:592-640`; `internal/effects/goodqa.go` (readback by the key trailer), `publish.go:511` (readback of the remote ref).
Tests: `TestGoodQALostResponseReconciles`, `TestPublishConfirmNeedsReadback` (mutant `M302`), `TestPublishMovedHeadQuarantined` (`M284`), `TestPublishHeadMovesDuringDispatch` (`M285`), `TestStaleDispatchedRecovers` (`M254`).
Limits: readback is only as good as the remote's observability, and the app role is the reconciler: README states it settles a `RECONCILING` effect "to whatever readback the app reports".

**R14f. Operator-only settlement with audit.**
Threat: a quarantined effect is cleared by a runner, or without a trace.
Invariant: only the app role (not the runner) can resolve a `QUARANTINED` effect to `RESOLVED_APPLIED` or `RESOLVED_NOT_APPLIED`, with a non-empty reason, evidence reference and actor, and the state change and an `effect_resolutions` row commit together. A never-dispatched intent is abandoned the same way. `effect_resolutions` is append-only (UPDATE, DELETE and TRUNCATE raise) and its `db_user`, `xact` and `at` columns are stamped by trigger, not by the writer.
Enforced at `broker.go:797-838` (`ResolveQuarantined`), `:846-` (`Abandon`), `0020_effect_resolutions.sql` (table, `CHECK (reason <> '')`, stamp and append-only triggers, the outcome guard requiring an audit row from the same transaction).
Tests: `TestResolveQuarantinedEffect` (mutant `M212`), `TestQuarantinedEffectOperatorResolve` (`M215`, reducer), `TestAbandonStrandedIntent` (`M226` to `M231`, `M253`), `TestAppRoleCannotBypassEffectOutcomes` (`M219`).
Limits: the audit's `actor` is the caller's self-asserted string (R20). `db_user` is the real database role, so the audit shows which credential did it. The table owner can disable triggers; there is no hash chain or external anchor ([Known gaps](#known-gaps), G-9).

**R14g. Outcome guard on every role.**
Threat: any client with write access to `effects`, including the app role, moves a state the broker never would.
Invariant: effects are born as a new intent only; every column except a short allow-list is immutable; state moves follow an allow-list that mirrors the broker's own steps; settled outcomes do not change except by retry; `dispatches` changes only by one on a dispatch; effects are never deleted or truncated; a new intent or move to `DISPATCHED` holds the live attempt row.
Enforced at `0020_effect_resolutions.sql:61-` (`fabric_effects_outcome_guard`, trigger `effects_zz_outcome_guard`) and `fabric_effects_hold_attempt`. The live body of the outcome guard is the one in `0024_publish_quarantine.sql:17-` and of the runner guard `0024:115-` (each migration replaces the previous body); runner limits were introduced in `0017`/`0018`/`0019`.
Tests: `TestEffectTransitionsAllowListed` (mutants `M246`, `M287`; M244 currently survives, see Evidence), `TestEffectColumnsImmutable`, `TestEffectInsertOnlyInitial`, `TestDispatchedHoldsAttempt`, `TestAppRoleCannotBypassEffectOutcomes`.
Strength: DB-enforced. Limit: README: the app role is the reconciler, so a `RECONCILING` effect settles to whatever the app reports; a `DISPATCHED` settle by a runner needs a receipt naming its dispatch number, which a runner can forge for its own live run.

**R14h. Dispatch lock and deadline.**
Threat: a hung remote holds a database connection and a subject forever; a reconciler reads back while the dispatcher is still sending.
Invariant: the dispatcher holds an advisory lock on the effect from before the `DISPATCHED` commit until the outcome is recorded; precondition plus transport run under a deadline (60 s or the attempt's remaining lease, whichever is shorter), stamped in `effect_dispatches`; at most half the pool dispatches at once; a dispatch is stale only when its lock is free, its attempt is not live, and it is older than twice the deadline.
Enforced at `broker.go:305-345` (slots, `pg_try_advisory_lock` at `:324`), `:462-` (deadline stamp), `:640-`; `0023_effect_dispatch_deadlines.sql`.
Tests: `TestInFlightDispatchNotReconciled` (mutant `M259` currently times out, see Evidence), `TestTransportTimeoutSettlesUnknown` (`M268`), `TestStaleFollowsStampedDeadline` (`M275`), `TestSlowDispatchesDoNotStarvePool` (`M266`, `M267`), `TestDispatcherSessionLossKeepsOutcome`.
Limits: the lock is session-scoped, so a transaction-mode connection pooler (PgBouncer) is unsupported (README). Deploy one broker version per database.

**R14i. The publish executor (git push) has its own gates.**
Threat: a push that overwrites someone else's work, goes to a different repository, or runs repository-supplied code.
Invariant: kind `publish` always needs approval; the push is a compare-and-swap on the expected remote head (a moved head quarantines); only a fast-forward is allowed; the destination and git directory are bound; git runs from a Fabric-owned mirror with an allow-listed environment, no hooks and no repository configuration; a push is confirmed only by readback.
Enforced at `internal/effects/publish.go:125` (approval), `:412` (ancestry check), `:445` (precondition), `:468` (dispatch), `:511` (readback), `riskyConfig` and `gitEnvKeep` near the top of the file; `0024_publish_quarantine.sql`.
Tests: `TestPublishValidate` (`M294`), `TestPublishMovedHeadQuarantined`, `TestPublishRefusesNonFastForward` (`M283`), `TestPublishBindsDestination`, `TestPublishBindsGitDir` (`M296`), `TestPublishEnvAllowListed` (`M291`), `TestPublishGitHelpersDisabled` (`M290`), `TestPublishQuarantineGuard`.
Limits: the env allow-list work for the worker side is still open (see G-6).

### R15. Native loop spend caps

- **Threat.** A runaway or manipulated agent loop spends without bound on a paid model.
- **Invariant.**
  - Three ceilings, fixed in code: USD 5 per job, USD 20 per UTC month, USD 1 per live test, with the per-run cap not above the job cap. A grant that names a higher cap is refused.
  - A worst-case cost (request body size plus `max_tokens` at the price table) is held against all caps, all or none, **before** any byte is sent; if any cap cannot fit it, nothing is sent. The actual cost then settles the hold. A hold whose process died is charged at its held amount.
- **Enforced at.** `internal/native/native.go:34-36` (constants), `:122-129` (grant validation); `internal/agent/ledger.go:93` (`Hold`, under per-resource advisory locks), `:136` (`Settle`), `:154` (`Reap`); `internal/agent/prices.go:83` (`WorstCase`); `internal/agent/loop.go:335-336` (hold before send); the reservations tables and triggers (`0009`, `0010`, `0014`) underneath.
- **Why deterministic.** Integer micro-USD arithmetic in a ledger over Postgres reservations, with row locks and a unique-live-reservation index. No model judgement.
- **Fails if broken.** `TestGrantMonthCeiling` (`./internal/native`; failing control C2), `TestMonthCeilingStopsLoop`, `TestBudgetExhaustedSendsNothing`, `TestBudgetHeldBeforeSend`, `TestBudgetCumulative`, `TestHoldSurvivesCrash`, `TestTransportAfterWriteCharged` (`./internal/agent`), plus the reservation mutants `M46` to `M60` (declared, not re-run).
- **Limits.**
  - The ceilings are constants in the binary: a rollback to an older binary restores its older ceilings (README).
  - The price table comes from configuration; a wrong table is wrong accounting. Spend is computed from reported usage and is not reconciled against the provider's invoice.
  - The month ledger is per database. A second database, or a deleted `usage_receipts` row by the table owner, resets it.
  - The `claude -p` worker path carries its own `MaxBudgetUSD` flag handled by the provider CLI, not by this ledger.

### R16. Credential and key-shape checks

- **Threat.** A provider key or token leaks into a log, a transcript, a fixture or a child process.
- **Invariant.**
  - The provider key is read by running a configured command, must be exactly one token, is cached for a bounded time, and its stderr is discarded; it never appears in the grant file.
  - The GoodQA key file must exist with mode 0600.
  - Tool output is passed through a redactor with 19 secret shapes before it is stored or returned to the model; a captured fixture that matches a secret shape or the key itself is not written.
  - Children of native tools never inherit the runner's environment.
- **Enforced at.** `internal/provider/anthropic/keysource.go:21-57` (single-token check at `:52`); `cmd/fab/effect.go:50-52`; `internal/policy/redact.go:24-48`; `cmd/fab/native_capture.go:88-90`; `internal/tools/exec.go` (Confiner owns the environment).
- **Why deterministic.** Regular expressions and file-mode checks. They are a deny-list: they reduce leakage, they do not prove its absence.
- **Fails if broken.** `TestKeyFromCommand` (`./internal/provider/anthropic`; failing control C9), `TestKeyNeverLeaks` (`./internal/agent`), `TestRedactorShapes`, `TestAlwaysOnSubset` (`./internal/policy`).
- **Limits.** The 0600 key-file check has no direct test (not yet tested). The `claude -p` worker still receives a runner DB DSN in its environment (README). Redaction is regex recall, not a guarantee; an unknown token shape passes through. There is no egress policy to stop exfiltration of what is not redacted (G-4).

### R17. Host sandbox profiles

- **Threat.** A worker reads credentials, writes outside its worktree, escapes its resource limits, or survives its own stop.
- **Invariant.** A launch happens only from a validated grant (model, effort, rung, tier-table hash, profile, caps, absolute paths, token); the launch line is built only from the grant; each profile proves its boundaries with negative controls before it advertises them.
  - **macOS (Seatbelt).** Own session; profile is `(allow default)` then `(deny file-write*)` with write allowed only to the run directory, temp and (for P1) the worktree, then a deny-read list (`.ssh`, `.aws`, `.claude`, keychains and similar); an allow-listed environment; inventory of the process tree by token and by an inherited descriptor. The Mac refuses writer (P1) grants for real runs.
  - **Linux.** `systemd-run --user` unit with `MemoryMax`, `CPUQuota`, `TasksMax`, Landlock filesystem rules and TCP connect limited to allowed ports (ABI 4), stop verified by cgroup emptiness.
- **Enforced at.** `internal/host/grant.go:43` (`Validate`), `internal/host/darwin.go:39-72` (profile), `:81` (P1 refused), `:356` control `environment_is_allow_listed`; `internal/host/linux.go:89-160`, `internal/host/landlock_linux.go:118-165`.
- **Why deterministic.** Kernel enforcement (Seatbelt, Landlock, cgroups) rather than model judgement. The controls make the claim falsifiable on every host.
- **Fails if broken.** `TestGrantValidateFailsClosed`, `TestNegativeControls` (set `FABRIC_HOST_CONTROLS=1` and, on Linux, `FABRIC_FAB_BIN`), `TestRV12_StopFailsClosed`, `TestDarwinRefusesWriterGrants` (macOS only), `TestArgvFromGrantOnly` (mutant `M21`).
- **Observed on Linux (same UID, an ordinary Linux host), this review.** Eight controls pass; two fail by design and are reported as failing: `controlling_user_manager_unreachable` (a worker can start a sibling unit through the user manager's bus, outside its cgroup and Landlock domain) and `controlling_user_process_unsignallable` (it can signal same-UID processes). The harness prints `containment demonstrated: false`. No real provider runs on Linux until the dedicated-account repair passes re-review.
- **Limits.**
  - macOS: `(allow default)` means network and process execution are not restricted, and reads outside the deny-list are allowed. It is a write-confinement and secret-path deny profile, not a full sandbox. `sandbox-exec` is deprecated by Apple (see [sandboxing notes](sota/sandboxing.md)). The macOS profile has no automated test in the Linux run above (it needs a Mac).
  - Linux: Landlock ABI 4 has no signal or abstract-socket scoping; network control is TCP connect/bind only.
  - No Windows backend (G-3). No egress policy beyond Linux TCP port limits (G-4). No per-run containerisation or microVM tier.

### R18. Database role separation (app vs runner)

- **Threat.** A compromised worker holds a database credential and rewrites state, approves its own effects, ends its own attempt, or schedules a host job.
- **Invariant.** Roles are `<prefix>_scheduler` (NOLOGIN, marks who may write scheduler events), `<prefix>_app` (fabd and fab; a scheduler member) and `<prefix>_runner`. No role owns a table, can `TRUNCATE`, or can `CREATE` in the schema. The runner role writes only: `effects` (INSERT, UPDATE) under column and state guards, `asks` (INSERT), `effect_dispatches` (INSERT), and UPDATE on `jobs` for a legacy exit record. Approval, readback and answers need the app role. Every statement and idle transaction is bounded (60 s).
- **Enforced at.** `internal/store/roles.go:35` (`runnerWrites`), `:39` (`RoleTimeout`), `ApplyRoles`; triggers in `0016_scheduler_role.sql` (`fabric_is_scheduler`), `0017_runner_write_guards.sql`, `0018_runner_transitions.sql`, `0019_runner_settle_live.sql`.
- **Why deterministic.** Postgres privileges and triggers; the guard is evaluated by the database for the session role, not by Fabric code.
- **Fails if broken.** `TestRolesLeastPrivilege` (mutants `M176`, `M179`, `M182`, `M183`, `M197`), `TestRunnerCannotApproveEffects`, `TestRolesUpgradeNarrowsRunner` (`M180`), `TestRunnerCannotShadowGuardTables`, `TestRunnerRoleBrokerFlows` (`./internal/effects`).
- **Limits (from the README, unchanged here).** The role is shared by every runner on a host, so any runner can act for any running mission; it can read every table; a forged transport receipt for a live run is believed; metrics and TUI connect with whatever `FABRIC_DSN` names (no read-only role is provided); the worker still carries the runner DSN. Roles must be re-applied after each migration (`fab db roles`), or the app role gets `42501`.

### R19. Host jobs are disabled (goodqa#15082, migration 0025)

- **Threat.** A run requests a "job" whose argv the daemon launches outside the run's sandbox, a sandbox escape through the scheduler.
- **Invariant.** `fab job run` and `fab job watch` exit 3; the scheduler never launches a job; the runner role has no `INSERT` on `jobs`, and the guard trigger refuses a runner insert even if the privilege is granted again by hand; a legacy watcher can still record `STARTED` to `EXITED` with identity columns unchanged; the daemon retires its own host's `REQUESTED` rows to `FAILED_TO_START`.
- **Enforced at.** `internal/sched/sched.go:41-44` (`ErrHostJobsDisabled`), `:942` (`RequestJob` refuses), `:950` (`retireRequestedJobs`); `internal/store/migrations/0025_runner_job_insert_guard.sql:11-25`; `internal/store/roles.go:35`.
- **Why deterministic.** Two independent layers (privilege and trigger) plus a Go refusal.
- **Fails if broken.** `TestRequestedJobNeverLaunched`, `TestRequestJobRefused` (`./internal/sched`), `TestRolesLeastPrivilege` (`./internal/store`; failing control C7: replacing the trigger's `RAISE` with `RETURN NEW` makes it fail), `TestForgedJobExitDoesNotWake`.
- **Limits.** Typed, approved job templates are future work, so a run cannot start work that outlives it (a designed capability loss).

### R20. Operator actions need an attributed actor and a reason

- **Threat.** A park, pause or quarantine is cleared casually by an automated component, or without a record of who and why.
- **Invariant.** Clearing a park (`fab admit`), unpausing, and resolving a quarantined effect or abandoning an intent need `FABRIC_ACTOR=op:<name>` and a non-empty `--reason`. The default CLI actor `chat`, and `fabd`, `native-runner`, `import` and `asks`, are refused for these commands. The actor and reason are stored in the event or audit row.
- **Enforced at.** `internal/domain/ids.go:71` (`IsOperator`: prefix `op:` and a non-empty name), `cmd/fab/main.go:172` (`actor()`), `internal/store/store.go:409` (`Operator: domain.IsOperator(actor)`), `internal/reducer/reducer.go:98`, `:117`; `broker.go:797`, `:846`.
- **Why deterministic.** An allow-list on the actor prefix, evaluated in the pure reducer.
- **Fails if broken.** `TestParkedAdmitIsOperatorOnly`, `TestUnpauseIsOperatorOnly`, `TestParkedAdmitRefusedForAutomationActors`; failing controls C3 and C4.
- **Limits.** **This is attribution and friction, not authentication.** Any process that can set the environment variable passes. The real separation is the database role. There is no operator login, second factor or per-operator credential (G-5).

### R21. Command idempotency and an append-only event log

- **Threat.** A retried or concurrent command applies twice; history is rewritten; a newer log is mangled by an older binary.
- **Invariant.** One transaction per command; the same key and the same canonical bytes replay the stored response, the same key with different bytes is `IDEMPOTENCY_CONFLICT`; per-mission event sequence numbers are gap-free; `events` and `commands` reject UPDATE, DELETE and TRUNCATE; an older binary refuses to write to a mission whose log holds an event kind it does not know; migrations record a checksum and a changed migration is refused.
- **Enforced at.** `internal/store/store.go:261` (`Execute`), `:330-362` (replay), `:365-371` (foreign event kind), `:104-118` (`Migrate` checksum); `0011_command_bytes.sql:38-44` (append-only and no-truncate triggers).
- **Why deterministic.** Primary keys and triggers; the reducer is replayed from the log (`reducer.Replay`, `:834`) and a rebuild must equal the stored snapshot.
- **Fails if broken.** `TestCommandReplay` (mutant `M11`), `TestConcurrentSameKey`, `TestEventSeqGapFreeAndRebuild`, `TestCommandRowChecks` (`M105`, `M107`, `M108`), `TestMigrateRefusesChangedMigration` (`M241`), `TestUnknownEventKindMissionGetsNoWrites` (`M139`), `TestVerifiedSnapshotRejectsUnknownEvent`.
- **Limits.** The table owner (migrations) can disable the triggers. There is no hash chain or external anchor over the log, so tampering by the owner is not detectable (G-9).

### R22. Ordered outbox

- **Threat.** A consumer skips an event whose transaction committed late, so a wake is lost.
- **Invariant.** An outbox page returns only rows whose transaction id is below the oldest in-progress transaction (`pg_snapshot_xmin`), ordered by id, with a per-consumer cursor. Wake notifications are hints; the table is the record.
- **Enforced at.** `internal/store/store.go:745-770` (`OutboxPage`).
- **Why deterministic.** A Postgres snapshot predicate, not timing.
- **Fails if broken.** `TestOutboxPagesAndCursor`, `TestRV3_OutboxDeliversLateCommit` (mutant `M34`).
- **Limits.** Delivery is at-least-once; consumers are expected to be idempotent (`CmdWake` is, `reducer.go:397-411`).

### R23. Native tool confinement and the policy oracle

- **Threat.** Prompt injection or a bad plan makes the native loop read outside the worktree, run a shell command that escapes, or approve itself.
- **Invariant.** `read` and `grep` are confined to the worktree by construction (path resolution, swap-race checks); `shell` runs in the host sandbox through a Confiner with its own environment, under an anchored allow-list; every shell call is first sent to external guard hooks (the "oracle"), which **fail closed**: a missing, timed-out or unexpected hook answer denies. The oracle is self-tested at start and the whole grant (allow-list, hooks, caps, turn and token limits) joins a configuration hash; a resumed run with a different configuration is refused.
- **Enforced at.** `internal/tools/read.go`, `internal/tools/exec.go`, `internal/policy/oracle.go:17`, `:128` (`SelfTest`), `internal/agent/loop.go:486` (`Guard.Check`), `internal/native/native.go:173-176`, `:331` (config hash).
- **Why deterministic.** Path and regex checks plus process exit codes. The oracle's rules live in the hooks, which are code, not a model.
- **Fails if broken.** `TestReadConfinement`, `TestReadSwapRace`, `TestShellAllowlistIsAnchored`, `TestNativeToolsConfined`, `TestShellOutputFloodStopped` (`./internal/tools`); `TestOracleFailsClosed`, `TestOracleSelfTest`, `TestInjectionCannotApprove`, `TestOracleDenyShapes` (`./internal/policy`); `TestConfigChangeRefused` (`./internal/agent`).
- **Limits.** The oracle is only as strong as the hook scripts it calls; those scripts live outside this repository, which makes this gate partly **Convention**. There is no taint tracking of tool output (see [gate angles](gate-angles.md), prompt-injection).

### R24. Provenance of asks, missions and answers

- **Threat.** A mission or an answer is attributed to a human when an agent invented it.
- **Invariant.** A mission must name a ticket; an answer must carry a source in a fixed grammar (`user:`, `ticket:`, `wal:`, `file:`, `inferred:`); a wake for an ask is accepted only for an answered ask of the same mission; a stored wake cannot be poisoned by forging one before the answer.
- **Enforced at.** `internal/store/catalog.go:30` (`ValidSource`), `internal/store/asks.go:72-`, `internal/store/store.go:373-388`.
- **Why deterministic.** A regular-expression grammar and row checks.
- **Fails if broken.** `TestMissionMustNameTicket`, `TestCatalogAndAsks`, mutants `M23`, `M38`, `M67` to `M71` (selected tests in `./internal/store`).
- **Limits.** A grammar check proves the source is well formed, not that it is true (an agent can write `user:...` text). The strong check is process-side (the harness verifies user turns).

### R25. Read-only observation surfaces

- **Threat.** The metrics endpoint or terminal view becomes a write path, a data leak or an injection vector.
- **Invariant.** `/metrics` listens only on a literal loopback IP unless explicitly overridden, answers 403 to a Host that is a name or any request with an Origin header, runs one collection at a time in a read-only transaction with a statement and lock timeout, and labels only ids and enums. The TUI uses read-only sessions, strips control, C1, bidi and zero-width characters from database text, and never shows effect requests, prompts or errors.
- **Enforced at.** `internal/metrics/http.go:18`, `:49`; `internal/metrics/metrics.go:89` (`ReadOnly`); `internal/tui/clean.go:25`.
- **Fails if broken.** `TestCheckListen`, `TestHostileLabels`, `TestCollectIgnoresRowLocks`, `TestCollectLockTimeout` (`./internal/metrics`); `TestKeysOff` and the sanitiser tests (`./internal/tui`); mutants `M308` to `M335`.
- **Limits.** `--allow-remote` has no authentication. Four tests in these two packages fail at `bc5a6d3` for a reason unrelated to the gate (stale goldens, see Evidence).

## Development and change gates

These gates govern how Fabric itself changes. Unlike the runtime gates they are mostly
enforced by the maintainers' tooling and review process outside this repository, so their
strength is **Convention** unless a file in this repository enforces them. The repository
contains the artefacts: commit messages that cite review findings (for example "review N2 on
aecf3fa", "red-team F1-F8 on 9b2b80c"), the mutant suite, the deploy instructions in the
README, and this document.

### D1. Every change has a ticket and a written specification

- **Threat.** Work with no stated outcome cannot be reviewed against anything.
- **Invariant.** Every want, need, finding or bug has a tracked ticket. Before implementation the owner writes a specification containing an outcome, scope, numbered acceptance criteria (`AC-n`) each with a `Verify by:` line, numbered negative cases (`NEG-n (AC-n)`) and a risk list. Every requirement carries a source; an item that cannot be traced is dropped or labelled an assumption.
- **Enforced at.** Maintainer tooling and commit policy (outside this repository). In-repo: missions in Fabric must name a ticket and answers must carry a source (R24), and commit messages cite ticket numbers (for example `goodqa#15082`).
- **Strength.** Convention, with a hook that blocks non-compliant commits in the maintainers' environment.
- **Evidence in this repository.** `git log` at `bc5a6d3`; ticket numbers in code comments and migrations.
- **Limits.** A reader outside the project cannot verify the spec for a given commit; it lives in the tracker.

### D2. Two independent reviews of the specification, one rung above the author

- **Threat.** The author's blind spots become the specification.
- **Invariant.** An adversarial review (what could go wrong) and an acceptance review (does it meet the outcome) are performed by two different models, agents or people, each one capability rung above the author (ladder: a Sonnet-class author is reviewed at Opus-medium; Opus-medium at Opus-high; and so on). The author cannot be a reviewer. Editing the spec afterwards makes the reviews stale.
- **Enforced at.** Maintainer tooling. In this repository, `internal/ladder/ladder.go` implements the escalation rule used by Fabric itself: escalate one rung after two genuine failures; infrastructure failures do not count.
- **Fails if broken.** `./internal/ladder` tests (`TestLadder*`, mutant `M24_ladder_infra_counts`).
- **Strength.** Convention for the review requirement; code for the ladder rule.
- **Limits.** Reviewer independence is procedural. Two models from different vendors reduce but do not remove correlated error.

### D3. Failing-first tests and a non-vacuous proof of every change

- **Threat.** A test that cannot fail proves nothing.
- **Invariant.** For changed behaviour a test is first shown to fail against the prior code or a deliberately broken variant; deterministic seeds are reported; a test must not pass with zero cases. The repository's own mechanism is the **must-fail mutant suite**: `tools/mutants/mutants.json` holds 343 source edits, each tied to an invariant and to the test that must fail. A mutant is `KILLED` only if the run fails **and** the named test is among the failures; `SURVIVED`, `BUILD_FAIL` and `KILLED_OTHER` fail the gate. The runner refuses to start when the unmutated suite is red.
- **Enforced at.** `tools/mutants/run.py`; `internal/storetest/storetest.go` refuses to run without `FABRIC_TEST_DSN` ("a green run that never touched the database would be vacuous"); `tools/native-mutants/` for the native loop; property tests with seeded generators in `internal/reducer`.
- **Strength.** Code, but run by hand or by CI; nothing in this repository forces a run before merge.
- **State at `bc5a6d3`.** The gate is currently **red and unrunnable**: the unmutated baseline fails (`TestNoModelLiteralsInSource`, see Evidence), so `run.py` exits with `BASELINE FAIL` and judges no mutant. Until fixed, a reader must run mutants with `--only` on packages whose baseline passes, as this review did.
- **Limits.** Mutants are hand-written. 343 mutants do not enumerate every defect class; the new gates T2, T3, T6, T7, the operator checks and the month cap have no mutants in `mutants.json` (this review added throwaway controls for them; see Evidence).

### D4. Red-team rounds before merge

- **Threat.** The author and the plan reviewers share assumptions and miss attack paths.
- **Invariant.** A substantive change gets an independent adversarial review that tries to falsify the change (bypass, failure mode, missing threat, scope gap). Material findings are fixed or explicitly downgraded and a second round follows if the fix changes behaviour. Commit messages record the finding ids (`F1` to `F8`, `H1`, `N2`, `T1` to `T7`).
- **Enforced at.** Maintainer process and the review helper tooling. The review evidence for this document is in the work directory named in the commit message.
- **Strength.** Convention.
- **Limits.** A review is a sample, not a proof. Findings the reviewer cannot reproduce from the packet are not found.

### D5. Independent click-test of the deployed build

- **Threat.** The tests pass and the deployed build does not work.
- **Invariant.** A tester who is not the author, a spec reviewer or the evidence poster runs the deployed build, and the report (identity, start and end time, deployed commit, logs) is attached to the ticket. In Fabric this is mirrored by R13: no `COMPLETE_MISSION` without a live-test receipt for the deployed commit.
- **Strength.** R13 is Code-enforced (the receipt must exist and bind to the commit); who performs the test is Convention.

### D6. Secret scan before any push

- **Threat.** A key, token or private host detail is published in a public repository.
- **Invariant.** The branch is scanned for secret shapes and private identifiers by a gate before it is pushed to the public remote; the push is made by a single controlled actor, not by the worker.
- **Strength.** Convention plus tooling outside the repository. In-repo analogues: the redactor (R16) and the rule that fixtures matching a secret shape are not written.
- **Limits.** A scan is a deny-list; it cannot prove absence. This document follows the same rule: no hostnames, addresses or private ticket text.

### D7. Deploy with backup, level match and rollback

- **Threat.** A deploy corrupts data, installs the wrong build, or cannot be undone.
- **Invariant (from the README "Deploying").**
  1. Back up with `pg_dump -Fc`, verify with `pg_restore --list`, keep the previous binary.
  2. Build from a fresh `git clone` at the release commit with `-buildvcs=true`; refuse to install a build that prints `devel` or ends in `-dirty`; the printed 12-character prefix must equal the commit.
  3. Stop everything using the database, run `fab migrate` as the owner, then `fab db roles` as the owner with the same password files.
  4. Restart and smoke-test: `fab version` and `fab metrics` (`fabric_schema_level` equals the newest migration).
  Rollback: reinstall the previous binary and restore the dump. The spend ceilings are constants, so a rollback also restores the previous ceilings.
- **Enforced at.** README steps (Convention); in code: `Store.Migrate` refuses a changed migration (R21), `fab version` prints the build revision, `fabric_schema_level` and `fabric_build_info` expose the level for the "level match" check.
- **Limits.** Migrations are roll-forward only (`0020`, `0025` say so); rollback is a restore, and everything written after the dump is lost. No automated deploy check lives in this repository.

### D8. Live worked example: the provider specification

Work on the native provider path (goodqa#15087, with the spend caps goodqa#15086) follows D1
to D7: a written specification with acceptance and negative cases, adversarial and
acceptance reviews, failing-first tests, a red-team round and a live test. Its open findings
are listed in [Known gaps](#known-gaps) until they ship, rather than being described here as
fixed.

## Evidence: failing controls and test results

All runs below were made on a Linux host against a throwaway PostgreSQL 16 container, with
the tree at `bc5a6d3`, on 2026-11-07. Nothing here touched a production system.

### Full suite at bc5a6d3

`FABRIC_TEST_DSN=... FABRIC_HOOKS_DIR=... go test -count=1 -v ./internal/... ./cmd/...`

| Package | Result |
|---|---|
| `agent`, `domain`, `effects`, `host`, `importer`, `ladder`, `native`, `policy`, `probe`, `projection`, `provider/anthropic`, `reducer`, `sched`, `store`, `tools`, `cmd/fab` | `ok` |
| `provider/claude` | **FAIL** `TestNoModelLiteralsInSource` |
| `metrics` | **FAIL** `TestExpositionGolden` |
| `tui` | **FAIL** `TestGoldenFrame`, `TestDrillDown` |

Every test cited in this document was found with `--- PASS` in that run, except the four
listed above, which this document does not rely on for a gate. They fail for two reasons that
are defects in the test side, not in a gate:

- `TestNoModelLiteralsInSource` scans the whole tree for model-name literals and flags
  `internal/agent/budget_test.go:214` and `:239`, the native-loop tests' own pattern and
  planted literal. This also makes the unmutated baseline red, so the mutant runner refuses
  to start (see D3 and G-10).
- The metrics and TUI golden files still show a month cap of `$100.00`; the code now enforces
  USD 20 (`1e8d80a`, `bc5a6d3`). The goldens were not regenerated (G-11).

### Host controls on Linux

`FABRIC_FAB_BIN=<built fab> FABRIC_HOST_CONTROLS=1 go test -count=1 -v -run 'TestNegativeControls|TestGrantValidateFailsClosed|TestRV12' ./internal/host`
printed `--- PASS` for all three, with these control results: pass: group signal misses
a `setsid` grandchild (as expected), cgroup stop reaps it, Landlock denies a write outside the
worktree, `MemoryMax` ends an allocation, secret paths unreadable, Claude config and caches
unwritable, TCP connect limited to allowed ports. **Fail by design on a same-UID host:**
`controlling_user_manager_unreachable`, `controlling_user_process_unsignallable`. The harness
prints `containment demonstrated: false`. This is the honest state of the Linux profile
(R17, G-8). No macOS run was made for this document.

### Repository must-fail mutants that were run for this review

`python3 -I tools/mutants/run.py --repo . --only <ids> --env FABRIC_TEST_DSN=...` (the
runner copies the tree, applies one source edit, runs the package, and requires the named
test to be among the failures). Selected so that the packages' baselines are green.

| Mutant | Gate | Outcome |
|---|---|---|
| M1, M3, M12, M17, M27, M34, M35, M120, M127, M203, M268, M294, M346 | R4, R1, R1, R12, R14b, R22, R5, R14a, R2, R13, R14h, R14i, R6 | Failed by the named test, as intended (13 of 17) |
| M184 `effects_runner_approves` | R14b (runner cannot approve) | **Survived**, reproduced on a second run. Probable cause: the edit targets the body in `0017`, which `0018`, `0019` and `0024` replace with `CREATE OR REPLACE`, so the edit is dead code in the live schema. The behaviour is still covered by `TestRunnerCannotApproveEffects`; the mutant no longer proves it. |
| M244 `transitions_fall_through` | R14g | **Survived**, reproduced. Same probable cause: the edited `0020` function body is replaced by `0024:17`. |
| M45 `hold_capacity_unlocked`, M259 `dispatch_no_lock` | R2, R14h | **Timed out at 600 s** on a rerun (first run 241 s); the runner reports `KILLED_OTHER` with no failing test. A hang is not a failure of the named test. Not resolved here. |

Reading: 13 of 17 behave as intended. The two survivors are most likely stale mutants (the
suite does not track a later `CREATE OR REPLACE` of the same function); two hang. This is
recorded as G-10 and G-23. Where a gate cites a survivor or a hang as its mutant, the test,
not the mutant, is the evidence.

343 mutants are declared; only those listed above were re-run here. The rest are "declared,
not re-run". The runner cannot run the full set on `bc5a6d3` until the baseline is fixed.

### Throwaway failing controls written for this review

The mutant file has no mutant for several gates added late (final admission, park and unpause
operator checks, sweep isolation, T2 re-park, settle retry, the 0025 trigger, the month cap, the
key-source single-token rule). For each, a script copies the tree outside git, applies one edit,
and runs only the cited test. A control is `KILLED` when the test fails with `--- FAIL`. None of
these edits is committed anywhere.

| Control | Edit in a throwaway copy | Test run | Result |
|---|---|---|---|
| C1 | `store/dispatch.go`: lease clause of `staleReason` replaced by `case false:` | `TestUngatedFinalAdmission` | KILLED |
| C2 | `native/native.go`: month cap constant 20 USD to 200 USD | `TestGrantMonthCeiling` | KILLED |
| C3 | `reducer.go`: park re-admission check replaced by `if false {` | `TestParkedAdmitIsOperatorOnly` | KILLED |
| C4 | `reducer.go`: unpause operator check replaced by `case false:` | `TestUnpauseIsOperatorOnly` | KILLED |
| C5 | `store/reservations.go`: sweep returns at first failing row | `TestSweepsIsolatePerRowFailures` | KILLED |
| C6 | `reducer.go`: `withRepark` call removed | `TestParkHeldWhileAnotherAttemptLive` | KILLED |
| C7 | `0025_runner_job_insert_guard.sql`: `RAISE EXCEPTION` replaced by `RETURN NEW` | `TestRolesLeastPrivilege` | KILLED |
| C8 | `sched.go`: settle loop breaks on the first failure | `TestSettleSurvivesOneTransientFailure` | KILLED |
| C9 | `anthropic/keysource.go`: multi-token key accepted | `TestKeyFromCommand` | KILLED |

9 of 9 were killed. Limit of this evidence: one edit per gate is a spot check, not a proof that
every way of weakening the gate is caught. Several controls (C5, C7) were killed by tests that
cover more than the one property.

### Gates with no direct failing test (not yet tested)

- The GoodQA key file mode check (`cmd/fab/effect.go:50-52`); `cmd/fab` has no test of it.
- The macOS Seatbelt profile (needs a Mac; not run for this review).
- `Store.SetSlotCap` has no audit (nothing to test).
- Clock-step behaviour of leases (G-14).

## Comparison and rationale

This section measures the design against published practice. "Ahead", "parity" and "behind"
are statements about the specific mechanism, with the evidence that supports them. A peer's
silence in its documentation is treated as "not documented", not as "absent". Fabric is a
single-host, early, shadow-stage system; many of the peers are production platforms with
scale, maturity and breadth Fabric does not have. Detailed per-component comparisons, with
ranked upgrades, are in [docs/sota/](sota/README.md). Sources are listed at the end of this
section with their access dates and the confidence of what was read.

| Area and reference | Published practice | Fabric today | Verdict |
|---|---|---|---|
| **Durable execution**: Temporal [S1][S2] | An ordered event history, with workflow code replayed against it; activities are at-least-once, and the called service enforces idempotency; history has size limits and Continue-As-New. | A per-mission append-only gap-free log with a pure reducer that is replayed to rebuild state (`reducer.Replay`, `TestEventSeqGapFreeAndRebuild`). Effects are not re-executed on replay. | **Parity** on the log-and-replay idea; **behind** on versioning of in-flight state across code changes, history-size policy and scale; **different scope** (Fabric records decisions, it does not re-run user code). |
| **Transactional outbox** [S3] | Write the message in the same transaction as the state change; the relay is at-least-once so consumers must be idempotent. | `events` and `outbox` rows share the command's transaction; pages exclude rows newer than the oldest in-progress transaction (`store.go:754`, `TestRV3_OutboxDeliversLateCommit`). | **Parity**. The xid-snapshot cursor is a documented fix for late commits; Fabric implements and tests it. |
| **Saga** [S3] | Multi-step business transactions with explicit compensating actions; no automatic rollback. | An effect has no compensation notion. A mission cannot undo a CONFIRMED effect; the operator resolves manually. | **Behind**. Not needed for the current effect kinds; a gap before multi-effect missions. |
| **Idempotency keys** [S4][S5] | Same key and different parameters is an error; a retry returns the original outcome. | Same key plus any different intent field is `IDEMPOTENCY_CONFLICT`; a settled effect replays after its attempt ends (`TestReplayBindsWholeIntent`, `TestTerminalReplayAfterAttemptEnds`). | **Parity**. No key-retention policy is stated (peers state 24 h pruning). |
| **Fencing tokens** [S6] | Leases alone are unsafe; the protected resource must reject writes with a lower token. | Fence checked in the reducer, in `Heartbeat`, in the broker (twice around the precondition) and in a trigger that locks the attempt row. | **Parity** inside the system boundary. **Behind** at the outside edge: the remote system gets an idempotency key, not the fence. |
| **Unknown outcomes** [S1][S5] | Retry the activity; the callee must be idempotent. | An ambiguous transport result is `UNKNOWN`; it is never retried; only a readback or an audited operator decision settles it. | **Ahead on safety** for effects whose remote cannot deduplicate; **behind on availability** (an unreadable remote blocks the mission until a human acts). A design choice, not a superset. |
| **Human approval of agent actions**: MCP tools spec, OWASP LLM06 [S7][S8] | "SHOULD" a human-in-the-loop for sensitive operations; show tool inputs; complete mediation downstream; log. | Approval is bound to the SHA-256 of the canonical request, rechecked at dispatch, and held by a DB trigger. But approval is optional per submission except for `publish`, and `fab effect comment --approve` approves its own submission. | **Ahead of the spec's requirement** on binding approval to exact content; **behind** on mandatory policy per effect kind and on separation of duties (G-1, G-2). The spec is a protocol requirement, not an implementation, so this is a comparison of strength of mechanism, not of products. |
| **Excessive agency** [S8] | Narrow tools, least privilege downstream, human approval for high impact, rate limits. | Confined read/grep/shell, a fail-closed oracle, DB role separation, caps. Approval optional, no per-effect rate limit. | **Partial**. |
| **Unbounded consumption** (LLM10) [S9] | Limit resource use per request and per user. | Worst-case hold before any send; run, job and month caps in code; crash charged at the held amount. | **Parity** with budget-limit gateways on the idea; the hold-before-send with crash charging is the part Fabric implements in the database. Not reconciled against invoices. |
| **Prompt injection** (LLM01) [S9] | Constrain behaviour, filter inputs and outputs, privilege control, human approval for high-risk actions. | The oracle blocks shell calls; confined tools; no tracking of untrusted content through the loop. | **Behind** on taint tracking (see [gate angles](gate-angles.md)). |
| **Supply chain** (LLM03) [S9] | Vet components, pin versions. | Dependencies are `pgx/v5` and `x/sys` only (README); no gate on packages an agent installs. | **Behind** on agent-installed packages. |
| **Capability and least-privilege sandboxes** | Network egress policy, filesystem confinement, per-run isolation, microVM tiers (see [sandboxing](sota/sandboxing.md)). | Landlock filesystem plus TCP-port limits, cgroups; Seatbelt write-confinement; no egress proxy; two Linux controls fail on same-UID hosts. | **Behind** peers that ship an egress policy and a VM tier; **parity** on kernel-feature use (Landlock, cgroups). |
| **NIST AI RMF 1.0 and the Generative AI Profile (600-1)** [S10] | Govern, Map, Measure, Manage. | Manage is the strongest (caps, parks, quarantine, audit). Measure is the weakest: no model-quality evaluation gates. | **Partial**; not a conformance claim. |
| **NIST SSDF (SP 800-218)** [S11] | Secure software development practices across preparation, protection, production and response. | Reviews, failing-first tests, must-fail mutants, a documented clean-clone deploy with build-version check. No SBOM, no vulnerability-disclosure policy file. | **Partial**; not a conformance claim. |
| **SLSA** [S12] | Build L1: provenance exists; L2: signed provenance from a hosted platform; L3: hardened builds. | The deploy procedure builds from a fresh clone and rejects `devel`/`-dirty`. No provenance is generated or signed. | **Below Build L1** (no provenance). The procedure is a good basis; the attestation is the missing artefact. |
| **Anthropic "framework for developing safe and trustworthy agents"** [S13] | Keep humans in control, transparency, secure agent interactions, privacy. | Park/pause/quarantine are human-held; operator attribution; visible state (TUI, metrics). | **Partial**; the human-control gates are real, operator authentication is not (G-5). |
| **OpenAI "practices for governing agentic AI systems"** [S14] | Practices include constraining the action space, legibility, automatic monitoring, attributability, interruptibility. | Action space: confined tools, broker. Attributability: actor plus real `db_user` in the audit. Interruptibility: pause, park and slot cap exist; no operator cancel (G-7). | **Partial**. |

**Where Fabric is ahead, in one place.** On three mechanisms, relative to what the cited
sources describe: (1) approval bound to exact request content and enforced again by the database
outcome guard; (2) a state that refuses automatic retry of an ambiguous effect and demands a
readback or an audited human decision; (3) a spend reservation made before a model request is
sent, charged at the held amount if the process dies. Each has code, a test and a mutant
(R14b, R14d, R15). None of the three is a claim about any specific product; they are claims
about the mechanisms the cited pages describe.

**Where Fabric is at parity.** Event-log-with-replay, outbox ordering, idempotency-key
semantics, fencing inside the system boundary, kernel sandbox primitives.

**Where Fabric is behind.** Compensation, versioning and growth policy for the log,
operator authentication, mandatory approval policy, separation of duties, egress control,
prompt-injection taint tracking, signed provenance, model-quality evaluation gates,
multi-host operation, Windows. These are listed as gaps below and in
[gate angles](gate-angles.md).

**Why this is a defensible position, and not a claim of leadership.** The gates that matter
for correctness are decided by constraints the database enforces for every connected role, by a
pure state machine that is replayed, and by locks and tokens, not by a model's judgement. Every
guarantee here names the code and a test, and 9 of 9 spot-check controls plus the mutants that
were run fail when the code is broken. The weakest points are the ones that depend on a person
following a rule (operator identity, reviewer independence, the oracle's external scripts) and
on a worker's honest evidence. Those are named in each gate's Limits, because a gate list that
omits its limits is marketing.

### Sources for this section

Accessed 2026-11-07. Confidence: H read in full or the relevant section; M read, secondary;
L search-result summary only (the page body was not read, or a fetch was refused).

- [S1] Temporal, Workflows: https://docs.temporal.io/workflows (H); Event history limits: https://docs.temporal.io/workflow-execution/event (H)
- [S2] Temporal, Activities: https://docs.temporal.io/activity-definition (H)
- [S3] Transactional outbox: https://microservices.io/patterns/data/transactional-outbox.html (H); Saga: https://microservices.io/patterns/data/saga.html (H); ordering in a Postgres outbox: https://event-driven.io/en/ordering_in_postgres_outbox/ (L, search snippet)
- [S4] Stripe, idempotent requests: https://docs.stripe.com/api/idempotent_requests (H)
- [S5] AWS Builders' Library, making retries safe with idempotent APIs: https://aws.amazon.com/builders-library/making-retries-safe-with-idempotent-APIs/ (H)
- [S6] Kleppmann, How to do distributed locking: https://martin.kleppmann.com/2016/02/08/how-to-do-distributed-locking.html (H)
- [S7] Model Context Protocol, tools (2025-06-18): https://modelcontextprotocol.io/specification/2025-06-18/server/tools (H); security best practices: https://modelcontextprotocol.io/specification/2025-06-18/basic/security_best_practices (H). A later revision (2025-11-25) exists and was not read.
- [S8] OWASP LLM06:2025 Excessive Agency: https://genai.owasp.org/llmrisk/llm062025-excessive-agency/ (H). OWASP, Agentic AI Threats and Mitigations v1.0 (Feb 17, 2025): https://genai.owasp.org/resource/agentic-ai-threats-and-mitigations/ (M: the landing page; the PDF's threat list was not read)
- [S9] OWASP Top 10 for LLM Applications 2025 (LLM01 Prompt Injection, LLM03 Supply Chain, LLM06 Excessive Agency, LLM10 Unbounded Consumption, and the rest): https://genai.owasp.org/llm-top-10/ (H for the list; the per-risk mitigations are paraphrased from the risk pages cited above and the list, M)
- [S10] NIST AI RMF 1.0 (Jan 26, 2023; functions Govern, Map, Measure, Manage; under revision) and Generative AI Profile NIST-AI-600-1 (Jul 26, 2024): https://www.nist.gov/itl/ai-risk-management-framework (M: function names were in alt text)
- [S11] NIST SP 800-218, SSDF version 1.1 (Feb 2022): https://csrc.nist.gov/pubs/sp/800/218/final (M: the page did not list the practice groups)
- [S12] SLSA v1.0 Build levels (page marked retired; v1.2 is current): https://slsa.dev/spec/v1.0/levels (H for L1 to L3 definitions; v1.2 text not read)
- [S13] Anthropic, framework for developing safe and trustworthy agents (Aug 4, 2025): https://www.anthropic.com/news/our-framework-for-developing-safe-and-trustworthy-agents (L, search summary; follow-up https://www.anthropic.com/research/trustworthy-agents)
- [S14] OpenAI, Practices for governing agentic AI systems (Shavit et al., 2023): https://openai.com/index/practices-for-governing-agentic-ai-systems/ (L: fetch refused with HTTP 403; practice names come from a secondary summary, https://verifywise.ai/ai-governance-library/governance-frameworks/openai-practices-governing-agentic-ai-systems)
- Component notes with further sources (Landlock, Claude Code sandboxing, OpenTelemetry GenAI conventions, Rekor and others) are cited in the files under [docs/sota/](sota/README.md).

## Known gaps

Honest, specific and current at `bc5a6d3`. "Ticket" is a GoodQA ticket number; `TBD` means a
ticket is drafted and will be linked here when it is filed.

| ID | Gap | Evidence | Consequence | Ticket |
|---|---|---|---|---|
| **G-1** | **Approval is opt-in per submission, not mandatory per effect kind.** Only kind `publish` refuses a submission without approval. A `ticket_comment` effect submitted without `--approve` never needs one. No table maps an effect kind to "approval required". | `effects/publish.go:125` (publish only); `cmd/fab/effect.go:132-144`; `broker.go:236`. | A worker (or anything holding the runner role, which can INSERT effects) can submit and dispatch an unapproved effect of any other kind. The approval gate protects only the kinds whose executor demands it. | TBD |
| **G-2** | **No separation of duties for approval.** `fab effect comment --approve` submits, approves and dispatches in one process; `approved_by` is any non-empty string. | `cmd/fab/effect.go:148-155`; `0020_effect_resolutions.sql` (`coalesce(NEW.approved_by,'') <> ''`). | The approval binds the request hash; it does not prove a second party approved. | TBD |
| **G-3** | **No Windows support.** | No `host` implementation for Windows. | Windows workers cannot be confined or run. | goodqa#15088 |
| **G-4** | **No egress policy.** macOS profile allows the network; Linux limits TCP connect/bind to allowed ports only. No DNS, protocol or destination policy, no proxy. | `host/darwin.go:61` (`allow default`); `host/landlock_linux.go:118`. | A worker (or injected content) can send any data it can read to any allowed destination. Secret redaction (R16) is the only mitigation. | TBD |
| **G-5** | **Operator identity is self-asserted.** `FABRIC_ACTOR=op:<name>` is an environment variable. | `domain/ids.go:71`, `cmd/fab/main.go:172`. | Any process on the host that can run `fab` can act as an operator. Audit rows record the string and the real DB role, not a person. | goodqa#15091 |
| **G-6** | **Open provider-path findings (goodqa#15087):** `LD_PRELOAD`-style loader injection and the completion of the worker environment allow-list. Until they ship, the `claude -p` worker path inherits more environment than intended and still receives the runner DB DSN. | README (Known limits); `sched.go:526` appends `FABRIC_DSN` to the worker env. | A worker can reach the runner role (R18 limits apply). | goodqa#15087 |
| **G-7** | **No operator cancel.** Interruptibility is pause, park and the slot cap, not a command that cancels a mission and stops its runner. | No `cancel` command in `cmd/fab`. | An operator cannot stop one mission cleanly except through the pause/quarantine path. | goodqa#15093 |
| **G-8** | **Linux sandbox same-UID escape.** On a same-UID host a worker can start a sibling unit via the user manager's bus and signal same-UID processes. | `TestNegativeControls` output in Evidence; `host/linux.go:20-26`. | No real provider runs on Linux until the dedicated-account repair passes re-review. | in the Linux repair; TBD |
| **G-9** | **Audit and event log are not tamper-evident.** UPDATE, DELETE and TRUNCATE are rejected by triggers, but there is no hash chain, signed checkpoint, separate audit-writer role or off-host copy; the table owner can disable the triggers. | `0011:38-44`, `0020`. | An owner-level compromise can rewrite history undetectably. | TBD |
| **G-10** | **The must-fail mutant gate does not run at `bc5a6d3`, and part of it is stale.** The unmutated baseline is red (`TestNoModelLiteralsInSource`), so `run.py` exits `BASELINE FAIL` on the full set. Of 17 mutants re-run on green packages, 2 survive (M184, M244: probably edits to function bodies later replaced by `CREATE OR REPLACE`) and 2 time out (M45, M259). | Evidence section. | The strongest failing-first control in the repository is unusable until fixed; 343 declared mutants are not re-run in CI-style use. | TBD (routed to the Fabric owner) |
| **G-11** | **Stale golden files** (`TestExpositionGolden`, `TestGoldenFrame`, `TestDrillDown`) after the month cap moved to USD 20. | Evidence section. | Three tests fail on a clean tree; a real regression in those packages could hide among them. | TBD (routed) |
| **G-12** | **`SetSlotCap` is a bare UPDATE** with no reason, actor or audit row. | `store/store.go:197-199`. | The admission limit can be changed silently by anyone with write access to the table. | TBD |
| **G-13** | **Final admission is check-then-start, not atomic.** The window is small but real; the ungated path stores no decision. | R3 Limits. | A runner can start with authority that expires moments later. The fence still stops its Fabric writes. | TBD |
| **G-14** | **No clock-step test for leases.** The model assumes the database clock does not move backwards. | No test. | A backwards step could extend leases. Postgres does not promise monotonic wall time. | TBD |
| **G-15** | **Evidence receipts are worker-asserted.** The arbitration gate proves a claim cites evidence for the current head, not that the evidence is true; there is no independent verifier. | `reducer.go:626-648`. | A worker that records a false but non-vacuous receipt passes the gate. | TBD ([gate angles](gate-angles.md)) |
| **G-16** | **Live-test receipts are caller-asserted.** `RecordLiveTest` trusts the caller's host, target and receipt strings. | `reducer.go:543-570`. | "Done" is as honest as whoever records the live test. | TBD |
| **G-17** | **Spend is not reconciled with the provider.** The price table is configuration; the month ledger is per database. | R15 Limits. | A wrong table or a second database defeats the month cap; the cap does not bound the real bill. | TBD |
| **G-18** | **macOS profile is write-confinement only.** `(allow default)` leaves network, process execution and non-listed reads open; `sandbox-exec` is deprecated by Apple. | `host/darwin.go:61`. | The Mac runs review (P0) only; no writer grants. | TBD |
| **G-19** | **Runner role is shared per host** and can read every table; a forged transport receipt for a live run is believed. | README (Known limits); R18. | One compromised worker can affect another running mission on the same host. | TBD |
| **G-20** | **The oracle's rules live outside the repository.** The native loop's shell guard calls external hook scripts. | `policy/oracle.go`. | Part of that gate is Convention; this repository cannot test the hooks' rules. | TBD |
| **G-21** | **No model-quality evaluation gates**: no held-out sets, no statistical promotion rule. Evals today are unit, property, mutant and negative-control tests. | [evals notes](sota/evals.md). | Model or prompt changes are not gated on measured quality. | TBD |
| **G-22** | **Single host; no placement, no shared-nothing multi-host scheduling.** | [multi-machine notes](sota/multi-machine.md). | Scale and failure domains are one machine. | TBD |
| **G-23** | **Mutants missing for late gates** (T2, T3, T6, T7, operator checks, month cap, 0025). Covered here only by throwaway controls C1 to C9. | Evidence section. | Regression of those gates depends on the named unit tests alone until mutants are added. | TBD |
| **G-24** | **Ambiguity of approval on retry.** A `FAILED_DEFINITE` or `NOT_APPLIED` effect re-arms under the same key with its approval kept (`APPROVED`) for the same request hash. The approval has no expiry or use count. | `0020` guard, `broker.go:664`. | An approval granted once covers every retry of the same exact request, indefinitely. | TBD |

Items that the README already states are repeated here deliberately, so that this document
is complete without it.

## Gate angles we do not yet cover

A research sweep (accessed 2026-11-07) of vendor agent-safety documents, OWASP, NIST, MITRE ATLAS,
SLSA and published incident reports found the gate types below. The full table with threats,
who does each today (cited), Fabric's coverage with evidence, a design sketch and a priority
is in [gate-angles.md](gate-angles.md). Headline: Fabric has partial coverage of
approval binding, spend circuit breaking, replay and kill switches, and none of prompt-injection
taint tracking, independent verification of effects, quorum approval, egress DLP,
agent-installed-package gates, signed provenance of agent outputs, drift detection and
multi-tenant isolation.

## Reproducing the checks

Setup (a disposable database, never a database you care about):

```
docker run -d --name fabric-docs-pg -e POSTGRES_PASSWORD=docs -p 127.0.0.1:55991:5432 postgres:16
export FABRIC_TEST_DSN='postgres://postgres:docs@127.0.0.1:55991/postgres?sslmode=disable'
go test -count=1 -v ./internal/... ./cmd/... 2>&1 | tee suite.log
grep -E '^(ok|FAIL|--- FAIL)' suite.log
```

One gate: `go test -count=1 -v -run '^TestApprovalBindsRequestHash$' ./internal/effects`.

Host controls (Linux; spawns only its own processes, bounded by its timeouts):

```
go build -o /tmp/fab ./cmd/fab
FABRIC_FAB_BIN=/tmp/fab FABRIC_HOST_CONTROLS=1 go test -count=1 -v -run TestNegativeControls ./internal/host
```

Selected mutants: `python3 -I tools/mutants/run.py --repo . --out /tmp/m.tsv --only M27_broker_approval_hash_unchecked,M1_stale_fence_overwrite --env FABRIC_TEST_DSN="$FABRIC_TEST_DSN"`.
The run starts with a baseline of every package the selected mutants touch and refuses to
continue if one is red.

Stop: `docker rm -f fabric-docs-pg`.
