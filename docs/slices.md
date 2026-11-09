# Slice spec outlines (F0 to F17)

These are outlines, not final specs. Each slice gets its own ticket and a full spec (outcome, scope, `AC-n` each with `Verify by:`, `NEG-n (AC-n)` cases, risks) before code. Specs for data, auth and migration slices need two reviews from different models or agents. The decision record is [adr/0001-kernel-scope.md](adr/0001-kernel-scope.md); the order is in [status-and-roadmap.md](status-and-roadmap.md).

Conventions used below:

- "Oracle" is the invariant oracle that already checks every reducer trace. "Mutant" is a must-fail mutation in `tools/mutants`.
- Every slice follows the interim rules in the ADR until F1 to F3 land.
- "Fixes" names the irreversible choice (ADR table) the slice decides.

## Lane order

| Lane | Order |
|---|---|
| Foundation | F1 envelope v2, F2 streams, F3 tenancy and RLS, F4 principals |
| Features | F6 harness adapters, F8 capability providers (browser, desktop), F5 fab-node, F9 message bus; then F7, F10, F11, F12; F13 last |

Cross-lane contract: the first feature PRs are pure packages behind an event-sink interface and carry no store coupling. Persistence arrives as typed envelope events once F1 merges, through a thin sink. Feature lane slices never wait for the foundation lane; they are re-pointed when it lands.

## F0 Scope ADR

- Outcome: the target, the role split and the irreversible choices are written down; README and roadmap describe the multi-tenant kernel.
- AC-1: ADR 0001 exists and names the seven irreversible choices (an eighth, the provider contract names, is added by ADR 0002). Verify by: review of `docs/adr/0001-kernel-scope.md`.
- AC-2: README scope statement no longer says "small control plane" and lists the 12 dimensions. Verify by: `grep -n "small control plane" README.md` returns nothing.
- AC-3: Roadmap lists F0 to F13 with dependencies. Verify by: review of `docs/status-and-roadmap.md`.
- NEG-1 (AC-2): no claim that a not-yet-built dimension is shipped. Verify by: every dimension row in the roadmap names its state and slice; review.
- NEG-2: no private customer or partner name, ops path, host name or DSN in the public repo. Verify by: a pattern scan of the diff before push.

## F1 Envelope v2 (foundation)

- Fixes: envelope fields and canonical bytes (1), id shape (2), reserved hash-chain columns (5).
- Outcome: events carry `schema_version`, `event_id`, `tenant_id`, principal plus issuer, `correlation_id`, `causation_id`, `trace_id` and `span_id`, and a typed payload per kind. An upcaster lifts v1 events. A minimum reader version protects rollback. An unknown kind parks its stream.
- AC-1: every stored v1 event rebuilds to byte-identical mission state through the upcaster. Verify by: replay test over a recorded v1 corpus, comparing state digests.
- AC-2: the canonical byte form is independent of Go field order. Verify by: test that shuffles struct field order and map insertion order and compares digests against fixed vectors.
- AC-3: an unknown kind parks that stream and fabd keeps running. Verify by: inject an unknown kind; the daemon keeps serving other streams and reports the parked stream.
- AC-5: every event carries a W3C `trace_id` and `span_id`. Verify by: a store test that rejects an envelope without them, plus a round-trip against fixed vectors.
- AC-4: an older binary refuses a newer envelope version rather than misreading it. Verify by: store test that raises the minimum reader version and starts the previous binary build.
- NEG-1 (AC-1): a mutant that drops the upcaster fails the replay test.
- NEG-2 (AC-3): a malformed payload for a known kind parks the stream with a reason; it does not partially apply.
- NEG-3 (AC-4): rollback to the previous binary at the same schema level still works (the deploy-rollback path is not broken).
- NEG-4 (AC-2): a duplicate JSON key or a non-finite number is rejected, not canonicalised.

## F2 Streams (foundation)

- Fixes: stream generalisation (4).
- Outcome: `stream_kind` plus `stream_id` replace the mission-only key; mission becomes kind `work_run`; the ticket becomes optional `external_refs`; command keys are per stream; the temporary ladder rows migrate to kind `ladder`.
- AC-1: ladder records live on `stream_kind=ladder` with no missions row. Verify by: store test and a query for missions with the ladder prefix returns zero.
- AC-2: the old `m_ladder-*` rows migrate without losing a receipt. Verify by: migrate a copy of a populated ladder database; receipt counts and digests match.
- AC-3: a work_run with no ticket is accepted and one with several external refs round-trips. Verify by: reducer and store tests.
- AC-4: the oracle passes on all existing property traces. Verify by: `go test ./internal/reducer/...` and the mutant run.
- NEG-1 (AC-2): re-running the migration changes nothing (idempotent).
- NEG-2 (AC-1): a ladder stream refuses work_run commands and the reverse.
- NEG-3 (AC-3): an external ref with an unknown system or malformed id is refused.

## F3 Tenancy and RLS (foundation)

- Fixes: tenant key (3).
- Outcome: `tenant_id` in all tables and keys, a `personal` default tenant, row-level security on the application and runner roles, global uniques rescoped.
- AC-1: two tenants, the runner role: a cross-tenant SELECT returns zero rows. Verify by: two-tenant store test.
- AC-2: the same idempotency key in two tenants is accepted and does not collide. Verify by: store test.
- AC-3: existing data lands in the personal tenant and replays unchanged. Verify by: migrate a copy of a populated database; state digests match.
- AC-4: a missing tenant context fails closed. Verify by: query with no tenant setting returns zero rows or an error, never all rows.
- NEG-1 (AC-1): a table added without RLS fails a catalogue test that lists every table.
- NEG-2: a superuser or owner role bypass is documented and the runtime roles are not owners.
- NEG-3 (AC-1): `SET ROLE` or a session setting from the worker environment cannot change tenant.

## F4 Principals and authority (foundation; merges the authority-proof design, goodqa#15135)

- Fixes: identity authority (6).
- Outcome: a local issuer and GoodQA token introspection produce principals; approvals are signed, expiring and bound to the request hash; an `op:` string is no longer trusted by itself.
- AC-1: a forged actor string cannot unpause or approve. Verify by: negative test with `FABRIC_ACTOR=op:x`.
- AC-2: an expired approval is refused. Verify by: test against the database clock.
- AC-3: an approval for request hash A cannot authorise request hash B. Verify by: effect broker test.
- AC-4: separation of duty: the requester cannot approve their own effect. Verify by: test with one principal on both sides.
- NEG-1 (AC-2): an approval replayed after use is refused (single use).
- NEG-2: a token for the wrong tenant, a revoked token and a token with a look-alike subject are refused.
- NEG-3: introspection outage fails closed and is reported, not treated as allow.

## F5 fab-node and machine registry (feature)

- Fixes: enrolled machine id `n_<ulid>` (2).
- Outcome: one `fab-node` per machine enrolls with a key, heartbeats by database time, advertises attested capabilities, takes placed work and takes part in leader election for sweeps. Replaces the per-host runner role.
- AC-1: an enrolled node gets an id that is not its hostname; renaming the host changes nothing. Verify by: enroll, rename, restart.
- AC-2: work is placed on a node whose attested capabilities match the request. Verify by: two nodes with different capabilities; placement test.
- AC-3: a node that stops heartbeating loses its leases and its work is re-placed without double execution. Verify by: kill a node mid-run; fences reject the old attempt.
- AC-4: exactly one leader runs sweeps at a time. Verify by: start three nodes; kill the leader; one successor within the lease window, never two.
- NEG-1 (AC-1): an unenrolled or revoked key cannot heartbeat or claim work.
- NEG-2 (AC-2): a node cannot advertise a capability it failed to attest (negative control digest mismatch).
- NEG-3 (AC-3): a clock-skewed node cannot extend its lease; only database time counts.

## F6 Harness adapters (feature, first)

- Fixes: none irreversible; defines the adapter interface that F7, F9 and F12 build on.
- Outcome: a harness registry replaces the closed provider enum. The adapter interface is `Start`, `Input`, `Interrupt`, `Stop`, `Events` (turn, tool call, message, usage, final, unknown) and `Caps` (resume, interactive, subagents). Adapters: native, claude-code interactive (stream-json input), codex (ported from the console).
- AC-1: the same conformance suite passes for native, claude-code and codex adapters using recorded fixtures. Verify by: `go test ./internal/harness/...` across all three.
- AC-2: `Input` reaches a running interactive session and `Interrupt` stops a turn without killing the session. Verify by: fixture session with scripted turns.
- AC-3: an unknown event from a harness becomes an `unknown` event, never a crash. Verify by: fuzz and fixture test.
- AC-4: adding a harness does not touch the reducer or the database enum. Verify by: a fake adapter registered in a test only.
- NEG-1 (AC-1): an adapter that omits a required capability fails registration.
- NEG-2 (AC-2): `Input` after `Stop`, double `Interrupt`, and `Input` during a pending turn are defined and tested (ordered, rejected or queued, never lost silently).
- NEG-3: harness stderr or output carrying a secret-shaped string is redacted before it becomes an event.
- NEG-4: a harness that exits without a final event settles as failed with the exit status, not as success.

## F7 Policy engine v1 (feature)

- Fixes: capability class names in use (7).
- Outcome: versioned policy bundles layered platform, tenant, org, project, user; decisions allow, deny or require approval; each decision is an event carrying the bundle digest; harness hooks call the decision point. Today's constants become platform ceilings.
- AC-1: a lower layer can narrow but never widen a higher layer. Verify by: layered bundle property test.
- AC-2: every decision is recorded with bundle digest, principal, class and subject. Verify by: event assertions.
- AC-3: a harness hook calls the decision point over a local socket and honours deny. Verify by: fixture harness.
- NEG-1: an unreachable decision point denies (fail closed).
- NEG-2: a bundle with an unknown class name or a malformed glob is refused at load.
- NEG-3: a bundle edit in flight does not change the decision for an already-approved request hash.

## F8 Capability broker and providers (feature, second)

- Fixes: capability class names (7).
- Outcome: invocations `{class, subject, scope, grant}` for browser and desktop use. Providers: an isolated browser engine, and the GoodQA desktop agent for a user's real desktop with consent. Fabric records grants, leases and artifacts with digests; the provider enforces.
- AC-1: a browser invocation without a live grant is refused. Verify by: broker test.
- AC-2: a lease expires and the session is torn down by database time. Verify by: test with a shortened lease.
- AC-3: a screenshot or page text is stored as an artifact with a SHA-256 digest, and a mutated artifact fails verification. Verify by: artifact test.
- AC-4: the browser provider and the desktop provider pass one shared conformance suite. Verify by: fake provider plus the real browser provider on a disposable page.
- NEG-1 (AC-1): a grant for tenant A cannot be used by tenant B or another principal.
- NEG-2: navigation to a host outside the grant's scope is refused (open redirects and look-alike hosts included).
- NEG-3: a desktop action without recorded user consent is refused, and revoking consent stops the session.
- NEG-4: provider crash mid-action leaves the invocation UNKNOWN and settled by readback, never assumed done.
- NEG-5: page content cannot give instructions that change the grant (prompt injection is data).

## F9 Message bus (feature, fourth)

- Fixes: none irreversible beyond ids and class names.
- Outcome: addressable mailboxes (`agent:`, `session:`, `human:`, `group:`), messages with from, to, thread and causation fields, delivery through `Adapter.Input`, and a bridge to GoodQA chat for human addresses.
- AC-1: a message sent to a running session reaches `Input` exactly once. Verify by: send with retries and a lost acknowledgement; one delivery.
- AC-2: a message to an offline mailbox is delivered after the session starts, in order. Verify by: ordering test.
- AC-3: a human address bridges to GoodQA chat and a reply returns as a message with the right thread. Verify by: staging round trip.
- NEG-1 (AC-1): a sender cannot send as another principal.
- NEG-2: tenant A cannot address tenant B's mailbox.
- NEG-3: a message body is data; it cannot carry a command that the receiving adapter executes without policy.
- NEG-4: mailbox growth is bounded and a full mailbox rejects visibly.

## F10 Per-node LLM gateway (feature)

- Outcome: a node-local proxy records model, request digest, usage and latency with the trace id; enforces budget holds, routing (the model ladder) and a kill switch; reconciles usage with the provider.
- AC-1: an over-budget call returns 429 and nothing is sent upstream. Verify by: fake upstream sees zero requests.
- AC-2: the kill switch stops in-flight and new calls. Verify by: test with a streaming response.
- AC-3: ladder routing picks the model the ladder function returns. Verify by: table test against the ladder.
- NEG-1: a concurrent burst cannot overspend a cap (hold before send).
- NEG-2: a worker cannot bypass the gateway by using another base URL (egress policy blocks it).
- NEG-3: request bodies are digested, not stored, unless the tenant opts in.

## F11 Trace API, export and hash chain (feature)

- Fixes: hash chain (5).
- Outcome: a read HTTP API over streams and events, OpenTelemetry export, and a per-stream hash chain over the F1 canonical bytes. The TUI and the console become clients of the API.
- AC-1: modifying one stored event row makes chain verification fail at that event. Verify by: tamper test.
- AC-2: the API returns only the caller's tenant. Verify by: two-tenant API test.
- AC-3: exported spans carry trace and span ids that match the stored envelope. Verify by: OTel collector fixture.
- NEG-1: deleting the last event of a stream is detected by an anchored head hash.
- NEG-2: API pagination cannot be used to read past a tenant boundary or skip events.
- NEG-3: API binds to loopback by default and requires a token off loopback.

## F12 Multi-agent orchestration (feature)

- Outcome: roles, child streams, fan-out and fan-in; the two-attempt limit becomes policy-driven.
- AC-1: a parent with N children settles deterministically when one child is cancelled. Verify by: oracle property test with seeded traces.
- AC-2: children inherit no more authority than the parent. Verify by: policy test.
- NEG-1: a child cannot outlive a cancelled parent's fences.
- NEG-2: fan-out is capped; exceeding the cap is refused, not truncated.
- NEG-3: duplicate completion of one child counts once.

## F13 Editions (feature, last)

- Outcome: personal edition (one binary plus Postgres, local issuer) and org edition (GoodQA-connected) pass the same smoke test.
- AC-1: a fresh install of each passes one shared smoke test. Verify by: scripted install on a disposable host.
- AC-2: the personal edition has no network dependency on GoodQA. Verify by: smoke test with the GoodQA host blocked.
- NEG-1: the personal local issuer is not reachable off loopback.
- NEG-2: org mode refuses to start with the local issuer enabled.

## F14 Memory and knowledge, F15 Evaluation and optimisation, F16 Agent definitions, prompts and flows

Added by the AWS agent-stack parity map ([aws-agent-parity.md](aws-agent-parity.md)). Their acceptance criteria, each with `Verify by:`, are defined there (AC-ME1 to AC-ME6 and AC-KB1 to AC-KB7 for F14; AC-EV1 to AC-EV6, AC-OP1 and AC-OP2 for F15; AC-AD1 to AC-AD4, AC-AG1, AC-AG2, AC-FL1, AC-FL2 and AC-PM1 for F16). The same page adds criteria to F1, F4, F5, F6, F7, F8, F9, F10, F11, F12 and F13, including the out-of-the-box criterion AC-OOTB on F13: a fresh personal install passes a parity smoke test with no extra wiring.

Each new slice follows the interim rules in the ADR (tenant and stream scope on every new table, typed versioned payloads, no fake-mission aggregates) and waits for F1 to F3 only where it needs the envelope, streams or tenancy.

## F17 Provider interfaces (cross-cutting, new)

Added by the Microsoft and Google parity map and [ADR 0002](adr/0002-provider-interfaces.md). Every component of Fabric has a first-party implementation behind a versioned provider contract (`fabric.provider/<kind>/v1`) with a conformance suite; a tenant binds a provider per kind (ours, AWS, Microsoft, Google or its own), and F7 policy and F11 trace wrap every provider call the same way. Fixes the provider contract names and descriptor fields (ADR 0001 choice 8). Its acceptance criteria, each with `Verify by:`, are AC-PI1 to AC-PI10 in [aws-agent-parity.md](aws-agent-parity.md). It is also a design constraint on F6, F8, F9, F10, F11, F14 and F15: each builds its first-party implementation behind the contract for its kind. A Google adapter has no live path: nothing in our deployments, CI or tests calls Vertex or aiplatform.
