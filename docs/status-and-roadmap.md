# Status and roadmap

Baseline: `main` at commit `df65c0d` (2026-11-08). Fabric is early and shadow-only: nothing here drives a production system yet, and no real provider runs on the Linux host profile until the sandbox repair passes re-review (see "Known limits" in the README).

The target is a multi-tenant execution and trace kernel under an organisation control plane, decided in [ADR 0001](adr/0001-kernel-scope.md). This page has three parts: the 12 target dimensions and where each stands, the slice plan (F0 to F13) with its two lanes, and the engineering-practice matrix that tracks how well the current core is built. Per-slice spec outlines, with acceptance checks and negative cases, are in [slices.md](slices.md).

## The 12 target dimensions

State is what the code at the baseline does today, not what is planned. "Slice" is the roadmap slice that closes the gap.

| # | Dimension | State at baseline | Slices |
|---|---|---|---|
| 1 | Multi-harness, including a built-in harness | partial: closed provider enum (`claude-code`, `anthropic-api`); a native loop with three tools; headless one-shot launch only | F6 |
| 2 | Multi-machine | stub: machine id is the hostname; one scheduler per host; one global capacity row | F5 |
| 3 | Multi-agent | stub: one writer and one reviewer per mission; no addressable agents or child work | F12, F9 |
| 4 | Policy configurable | stub: caps are code constants; policy shells out to harness hooks; approver is free text | F7, F4 |
| 5 | Traceable | partial: append-only per-mission events and effect resolutions; no trace ids, no hash chain, no export | F1, F11 |
| 6 | Long-term work tracking | absent: a mission must name a ticket; no other work types | F2 |
| 7 | Full computer use | absent | F8 |
| 8 | Full browser use | absent | F8 |
| 9 | Inter-agent and inter-harness messaging | absent | F9 |
| 10 | Gateway trace and control | stub: one egress destination; spend holds on the native loop only | F10 |
| 11 | Multi-tenant | absent: no `tenant_id`; single operator | F1, F3 |
| 12 | Multi-user and org-controlled, plus personal deploy | absent: actors are self-asserted | F4, F13 |

## Slices

Each slice has one owner, its own ticket when it starts, and `Verify by:` checks in its spec. "Fixes" names the irreversible choice from the ADR that the slice decides.

| Slice | Scope | Dimensions | Fixes | Verify by (headline) |
|---|---|---|---|---|
| **F0** Scope ADR | ADR, README and roadmap rewritten to the 12 dimensions | all | the target | ADR, README and roadmap in `main` |
| **F1** Envelope v2 | `schema_version`, `event_id`, tenant, principal plus issuer, causation, correlation and trace ids, typed payloads; upcaster; minimum reader version; unknown kind parks the stream | 5, 11 | envelope bytes, id shape | every stored v1 event rebuilds byte-identical state; a mutant that drops the upcaster fails; an unknown kind does not stop fabd |
| **F2** Streams | `stream_kind`; mission becomes `work_run`; ticket becomes optional `external_refs`; per-stream command keys; ladder rows migrate | 6, 3 | stream model | ladder records land on `stream_kind=ladder` with no missions row; old rows migrated; oracle passes |
| **F3** Tenancy and RLS | `tenant_id` in all tables and keys; personal default tenant; RLS on app and runner roles | 11, 12 | tenant key | cross-tenant SELECT returns zero rows under the runner role; same idempotency key in two tenants is accepted |
| **F4** Principals and authority | local issuer plus GoodQA token introspection; signed, expiring approvals bound to the request hash (merges goodqa#15135) | 4, 12 | identity authority | a forged `FABRIC_ACTOR=op:x` cannot unpause; an expired approval is refused |
| **F5** fab-node and machine registry | enrolment, heartbeat by database time, attested capabilities, placement, leader election | 2 | machine id | node rename changes nothing; killed node's work re-places without double execution; one leader at a time |
| **F6** Harness adapters | registry replaces the provider enum; interface `Start`, `Input`, `Interrupt`, `Stop`, `Events`, `Caps`; native, claude-code interactive, codex | 1, 3 | none | one conformance suite passes for all three adapters on recorded fixtures |
| **F7** Policy engine v1 | layered bundles, recorded decisions with bundle digest, harness hooks call the decision point | 4 | class names in use | lower layer cannot widen a higher one; unreachable decision point denies |
| **F8** Capability broker | `browser.*` and `desktop.*` invocations; isolated browser provider and GoodQA desktop agent provider; grants, leases, artifacts with digests | 7, 8 | capability class names | no grant means refused; lease expiry tears down by database time; mutated artifact fails digest check |
| **F9** Message bus | mailboxes, addressing, adapter delivery, GoodQA chat bridge | 9, 3 | none | exactly-once delivery under retry and lost acknowledgement; no cross-tenant addressing |
| **F10** Per-node LLM gateway | usage, budget 429s, kill switch, ladder routing | 10, 4 | none | over-budget call sends nothing upstream; kill switch stops streaming calls |
| **F11** Trace API and hash chain | read HTTP API, OTel export, per-stream hash chain; TUI and console become clients | 5, 10 | hash chain | one tampered row breaks chain verification; API returns only the caller's tenant |
| **F12** Multi-agent orchestration | child streams, fan-out, fan-in, role limits as policy | 3 | none | a parent with N children and one cancelled settles deterministically under the oracle |
| **F13** Editions | personal (one binary plus Postgres, local issuer) and org (GoodQA-connected) | 12 | none | a fresh install of each passes the same smoke test |
| **F14** Memory and knowledge | session and long-term memory, knowledge-base ingestion and retrieval | 5, 6, 11 | none | retrieval quality set and tenant isolation pass; see [aws-agent-parity.md](aws-agent-parity.md) |
| **F15** Evaluation and optimisation | evaluators, online and batch evaluation, LLM-as-judge, recommendations, A/B tests | 4, 5 | none | labelled fixture agreement thresholds met |
| **F16** Agent definitions, prompts and flows | declarative agent definition, immutable versions and aliases, prompt management, workflows | 1, 3 | none | one definition runs on two adapters with conforming traces |
| **F17** Provider interfaces | a versioned provider contract and conformance suite per component, descriptors with enforcement tiers, per-tenant bindings | 1, 3, 4, 5 | none | the same mission yields the same policy decisions and trace kinds with first-party and fake third-party providers; see [ADR 0002](adr/0002-provider-interfaces.md) |

### Execution plan: two parallel lanes

Features run in parallel with the foundation (decision recorded in the ADR). Neither lane waits for the other.

- **Foundation lane:** F1, then F2, then F3, then F4. These change data shapes, so each is a schema or contract slice with two spec reviews, a migration rehearsal on a copy of live data, and a rollback statement.
- **Feature lane:** F6 and F8 first, then F5 and F9. After those, F7, F10, F11 and F12 as their inputs appear; F13 last. The first PRs of the feature lane are pure packages behind an event-sink interface, so they carry no store coupling and move cheaply when the envelope and streams change.
- **Cheap-migration rules for both lanes** are in the ADR: tenant and stream scope columns on every new table, typed versioned payloads in one nested key, no new fake-mission aggregates, kind-prefix plus ULID ids, and capability class names used verbatim.
- **Leaving shadow-only:** per capability, when a slice's own live acceptance passes on the deployed build. Real work on live must use the interim-rule shapes (`tenant_id` leading key, versioned nested payloads, no fake missions).

### What is out of scope for this repository

Retiring the old per-host runners and rewiring the existing interactive console to the Fabric API are tracked under F5, F6 and F11 but are done in their own repositories. GoodQA's side (org and user management, ticket UI, token issuing) stays in GoodQA.

## AWS agent-stack parity

The requirement that anything the AWS agent stack can do, Fabric does out of the box is mapped capability by capability, with HAVE, PARTIAL and GAP status and acceptance criteria for every gap, in [aws-agent-parity.md](aws-agent-parity.md). Current count: 0 HAVE, 39 PARTIAL, 100 GAP of 139 capability rows (the AWS matrix plus the Microsoft and Google rows). The same page maps Microsoft Agent Framework and Foundry and Google's Agent Runtime and ADK, and defines the provider interfaces: every component has a first-party implementation behind a versioned, swappable contract ([ADR 0002](adr/0002-provider-interfaces.md), slice F17).

## Shipped (tested in the ways stated)

- Pure reducer with seeded property tests, an invariant oracle and must-fail mutants (`internal/reducer`, `tools/mutants`).
- Postgres store: idempotent commands, gap-free events, outbox, database-time leases and fences (`internal/store`, migrations through 0038).
- Effect broker with readback, quarantine and audited operator resolution (`internal/effects`).
- macOS and Linux host profiles with negative-control suites; fixed worker environment allow-list.
- Native `anthropic-api` loop with hold-before-send spend caps and an optional durable tool-call review gate.
- `fab` CLI, `fab tui` (read-only), `fab metrics` and `fab serve-metrics` (loopback by default).
- `fab done` refused until a live-test receipt exists for the deployed commit.
- Ladder receipts (goodqa#15324, S1b): review receipts are recorded as events and `fab ladder record` and `fab ladder status` derive a task's rung through `internal/ladder`. They are stored as temporary mission-shaped rows (`m_ladder-<task>`); F2 migrates them. The daemon does not yet route by the rung; that is F10.
- Binary-only deploy and rollback with a schema-level guard (goodqa#15348). It does not cover an envelope change; F1's minimum reader version does.

## Engineering-practice matrix (17 dimensions)

This matrix is separate from the 12 target dimensions above. It compares the current core with documented practice in other durable-execution and agent tools; the "Gap" column is the work still open. Result at `150bac7`: 2 has, 10 partial, 5 missing. The "Slice" column is added to show where the roadmap closes the gap; it does not change a verdict.

| Dimension | Fabric | Gap | Slice |
|---|---|---|---|
| Idempotency | has | Receipt retention policy | |
| Retries and backoff | has | No half-open circuit state (goodqa#15129); systemic errors not fast-tripped | |
| Durable execution | partial | No bounded history or checkpoint; agent loop is not replayed from history | |
| Determinism and replay | partial | Reducer replay is exact; LLM turns cannot be replayed, so a checkpoint-from-digest is needed | F10 |
| Human approvals | partial | Approval is not policy-driven (goodqa#15135); approver is self-asserted; no expiry or separation of duty | F4, F7 |
| Multi-host sandbox | partial | No host registry or placement; egress policy behind; Linux same-UID gap | F5 |
| Budgets and quotas | partial | Caps bypassable (goodqa#15123, #15124); no provider reconciliation; no per-tenant quota | F10, F3 |
| Model ladder | partial | Receipts recorded (goodqa#15324); nothing routes by the rung yet | F10 |
| Tracking UI and API | partial | CLI, TUI and metrics only; no HTTP read API or board | F11 |
| Audit trail | partial | Append-only, but no hash chain or off-system export; actor is self-asserted | F11, F4 |
| Secrets | partial | Provider key stays out of the sandbox; runner DSN is still in the worker environment; no short-lived per-run credentials | F3 |
| Eval and verify gates | partial | No verifier before `SUCCEEDED` (goodqa#15325) | |
| Versioning | missing | Unknown event kinds are refused; no upcasters or minimum reader version (goodqa#15133) | F1 |
| Scheduling | missing | No cron, timers, fairness or queue depth limit | F5 |
| Flags and canaries | missing | Static toggles only; no rollout, canary or auto-rollback | |
| Multi-tenant auth | missing | Single operator; tenant and principal id to be added to the event envelope (goodqa#15133); enforcement deferred | F1, F3, F4 |
| Authority proof | missing | No signatures, scope, expiry or authority chain (goodqa#15135) | F4 |

## Authority-proof approval

Principle: an action is allowed when it carries verifiable proof that an authorized principal sanctioned it. Approval is therefore not a flag the caller may omit and not a name the caller types. The design, tracked in goodqa#15135 and now slice F4, is an effect-class policy table keyed by kind and subject pattern that decides when approval is required, with approvals that are signed, scoped to the request hash, expiring, and checked for separation of duty. None of this is shipped. Today the request hash binding exists, and approval is otherwise opt-in (forced only for `publish`).

## Other open work

| Ticket | Outcome | Folds into |
|---|---|---|
| goodqa#15325 | Finished work moves to `VERIFYING`; only a verification receipt (check id, command digest, exit status, head sha) bound to the produced head reaches `SUCCEEDED`; a verifier cannot be the worker | stays a work_run feature; ports to F2 streams |
| goodqa#15135 | Authority-proof approval | F4 |
| goodqa#15133 | Event schema version, upcasters, minimum reader version, tenant and principal id | F1 |
| goodqa#15216, goodqa#15196 | Audited exit for stuck effects; macOS Seatbelt IPC gaps | independent hygiene |

Also open: the Linux same-UID sandbox repair.

## Next

Lane starts, in order: F0 (this change), then F1 in the foundation lane and F6 in the feature lane, with F8 next in the feature lane. Each slice ships only with a failing control, a focused test and a live check on the deployed build.
