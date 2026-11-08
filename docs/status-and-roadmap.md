# Status and roadmap

Baseline: `main` at commit `150bac7` (2026-11-08). Fabric is early and shadow-only: nothing here drives a production system yet, and no real provider runs on the Linux host profile until the sandbox repair passes re-review (see "Known limits" in the README).

## Shipped (tested in the ways stated)

- Pure reducer with seeded property tests, an invariant oracle and must-fail mutants (`internal/reducer`, `tools/mutants`).
- Postgres store: idempotent commands, gap-free events, outbox, database-time leases and fences (`internal/store`, migrations through 0038).
- Effect broker with readback, quarantine and audited operator resolution (`internal/effects`).
- macOS and Linux host profiles with negative-control suites; fixed worker environment allow-list.
- Native `anthropic-api` loop with hold-before-send spend caps and an optional durable tool-call review gate.
- `fab` CLI, `fab tui` (read-only), `fab metrics` and `fab serve-metrics` (loopback by default).
- `fab done` refused until a live-test receipt exists for the deployed commit.

## Capability matrix (17 dimensions)

Result at `150bac7`: 2 has, 10 partial, 5 missing. Each row compares Fabric with documented practice in other durable-execution and agent tools; the "Gap" column is the work still open.

| Dimension | Fabric | Gap |
|---|---|---|
| Idempotency | has | Receipt retention policy |
| Retries and backoff | has | No half-open circuit state (goodqa#15129); systemic errors not fast-tripped |
| Durable execution | partial | No bounded history or checkpoint; agent loop is not replayed from history |
| Determinism and replay | partial | Reducer replay is exact; LLM turns cannot be replayed, so a checkpoint-from-digest is needed |
| Human approvals | partial | Approval is not policy-driven (goodqa#15135); approver is self-asserted; no expiry or separation of duty |
| Multi-host sandbox | partial | No host registry or placement; egress policy behind; Linux same-UID gap |
| Budgets and quotas | partial | Caps bypassable (goodqa#15123, #15124); no provider reconciliation; no per-tenant quota |
| Model ladder | partial | `internal/ladder` exists but nothing in the daemon calls it (goodqa#15324) |
| Tracking UI and API | partial | CLI, TUI and metrics only; no HTTP read API or board |
| Audit trail | partial | Append-only, but no hash chain or off-system export; actor is self-asserted |
| Secrets | partial | Provider key stays out of the sandbox; runner DSN is still in the worker environment; no short-lived per-run credentials |
| Eval and verify gates | partial | No verifier before `SUCCEEDED` (goodqa#15325) |
| Versioning | missing | Unknown event kinds are refused; no upcasters or minimum reader version (goodqa#15133) |
| Scheduling | missing | No cron, timers, fairness or queue depth limit |
| Flags and canaries | missing | Static toggles only; no rollout, canary or auto-rollback |
| Multi-tenant auth | missing | Single operator; tenant and principal id to be added to the event envelope (goodqa#15133); enforcement deferred |
| Authority proof | missing | No signatures, scope, expiry or authority chain (goodqa#15135) |

## Authority-proof approval

Principle: an action is allowed when it carries verifiable proof that an authorized principal sanctioned it. Approval is therefore not a flag the caller may omit and not a name the caller types. The planned design, tracked in goodqa#15135, is an effect-class policy table keyed by kind and subject pattern that decides when approval is required, with approvals that are signed, scoped to the request hash, expiring, and checked for separation of duty. None of this is shipped. Today the request hash binding exists, and approval is otherwise opt-in (forced only for `publish`).

## In progress

All four tickets below are filed and in triage; no code has landed for them.

| Ticket | Slice | Outcome |
|---|---|---|
| goodqa#15324 | S1 | Review receipts become events; the daemon derives each task's rung through `internal/ladder`; `fab ladder status --task T` reports it. Infrastructure failures do not count; a new finding resets the count; the top rung needs a recorded user approval. |
| goodqa#15325 | S2 | Finished work moves to `VERIFYING`, and only a verification receipt (check id, command digest, exit status, head sha) bound to the produced head reaches `SUCCEEDED`. A verifier cannot be the worker. |
| goodqa#15135 | S3 | Effect-class policy table and authority-proof approval (above). |
| goodqa#15133 | S4 | Event schema version, upcasters, minimum reader version, tenant and principal id in the event envelope. |

Also open: the audited exit for stuck effects (goodqa#15216), the macOS Seatbelt IPC gaps (goodqa#15196) and the Linux same-UID sandbox repair.

## Next

Order follows the matrix: ladder wiring, verification receipts, authority proof, versioning, then scheduling (timers and fairness), a read API, hash-chained audit export, and flags with canaries. Each item ships only with a failing control, a focused test and a live check on the deployed build.
