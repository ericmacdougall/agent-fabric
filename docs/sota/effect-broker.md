# Effect broker: state of the art and where Fabric stands
Status line: pinned to commit bc5a6d3; sources accessed 2026-11-07.

## What this component does in Fabric
- Every external write (kinds `ticket_comment` and `publish`) is an effect row, not a direct call. States: INTENT, APPROVAL_REQUIRED, APPROVED, DISPATCHED, CONFIRMED, FAILED_DEFINITE, UNKNOWN, RECONCILING, NOT_APPLIED, QUARANTINED, RESOLVED_APPLIED, RESOLVED_NOT_APPLIED (`internal/effects/broker.go:29-41`).
- `Submit` binds the idempotency key to the whole intent (mission, attempt, fence, kind, subject, request hash, needs-approval); any change under the same key is `ErrIdempotencyConflict` (`broker.go:210-221`). Approval is opt-in per submission, except `publish`, which must need one (`publish.go:125`).
- `Approve` stores a hash only if it equals the request hash (`broker.go:283`). `Dispatch` recomputes the hash from the stored request before transport and checks fence, one live effect per subject, and approval (`broker.go:409-427`). The hash is `sha256(kind, canonical request)` (`internal/domain/canonical.go:62`); the subject is not in it but is immutable (migration 0017:60).
- A transport error or ambiguous answer becomes UNKNOWN and is never retried; `Retry` refuses UNKNOWN and RECONCILING (`broker.go:664-672`). `Reconcile` reads the remote back and moves to CONFIRMED, NOT_APPLIED, QUARANTINED, or stays UNKNOWN (`broker.go:592-638`).
- The remote key is the business key: GoodQA gets a deterministic uuid5 `operationId` (`goodqa.go:37`). QUARANTINED is settled only by an operator through `ResolveQuarantined`, which writes an `effect_resolutions` row in the same transaction; a trigger refuses either without the other (`broker.go:797`, `migrations/0020:1-30, 49-148`).
- There is no saga or compensation notion in the code: a search for saga, compensate and rollback found only database transaction rollbacks.

## State of the art
- Stripe v1 saves the first response under an idempotency key (including 500s), compares parameters on retry and errors on mismatch, and may prune keys after 24 hours; validation failures and concurrent-execution conflicts are not saved and can be retried. [Stripe](https://docs.stripe.com/api/idempotent_requests) (accessed 2026-11-07), H.
- AWS Builders Library: record the client token atomically with the mutation; return a semantically equivalent response on retry; reject the same token with different parameters; expire tokens eventually. After a timeout the outcome is unknown, and reconciliation is costly and may not be conclusive. [AWS](https://aws.amazon.com/builders-library/making-retries-safe-with-idempotent-APIs/) (accessed 2026-11-07), H.
- Temporal: an activity "may be executed multiple times"; authors make it idempotent, with keys enforced by the called service and suggested as workflow run id plus activity id; retries and backoff are tracked by the service. [Temporal](https://docs.temporal.io/activity-definition) (accessed 2026-11-07), H.
- Transactional outbox: record the message in the same transaction as the state change; the relay may publish twice, so receivers must be idempotent. [microservices.io](https://microservices.io/patterns/data/transactional-outbox.html) (accessed 2026-11-07), H.
- Saga: a sequence of local transactions where earlier steps are undone by compensating transactions; no automatic rollback, and concurrent sagas lack isolation. [microservices.io](https://microservices.io/patterns/data/saga.html) (accessed 2026-11-07), H.
- OWASP LLM06:2025 Excessive Agency lists excessive functionality, permissions and autonomy; prevention: minimal and narrow tools, least privilege downstream, human approval for high-impact actions, authorization enforced in downstream systems, plus logging and rate limiting as damage limitation. [OWASP](https://genai.owasp.org/llmrisk/llm062025-excessive-agency/) (accessed 2026-11-07), H.
- MCP 2025-06-18 tools: there SHOULD be a human in the loop able to deny invocations; clients MUST treat tool annotations as untrusted unless the server is trusted; servers MUST validate inputs, enforce access control, rate limit and sanitize outputs; clients SHOULD show inputs before calling, log usage and use timeouts. [MCP tools](https://modelcontextprotocol.io/specification/2025-06-18/server/tools) (accessed 2026-11-07), H. The security best practices page (fetched under the 2025-06-18 path but linking the 2025-11-25 authorization spec, so it may be the newer revision) forbids token passthrough and requires per-client consent and minimal scopes. [MCP security](https://modelcontextprotocol.io/specification/2025-06-18/basic/security_best_practices) (accessed 2026-11-07), M.

## Comparison
| Practice | Fabric today (file:line and test) | Verdict | Why |
|---|---|---|---|
| Same key, different body is an error | `broker.go:210-221`; `TestSameKeyDifferentBodyConflicts` (`broker_test.go:159`), `TestReplayBindsWholeIntent` (`identity_test.go:45`) | parity | Matches Stripe and AWS. Fabric binds more than the body, including attempt and fence, so a new attempt cannot replay an older attempt's key; it must use a new effect. |
| Key retention | Effect rows kept; no pruning code found | parity (peer not documented) | Stripe prunes after 24 hours and AWS expires tokens; Fabric keeps them, so there is no collision-after-expiry case but storage grows. |
| Unknown outcome handling | UNKNOWN is never retried, dispatch and retry refused, readback decides; `TestPostCommitLossReconcilesToConfirmed` (`broker_test.go:113`), `TestStaleDispatchedRecovers` (`resolve_test.go:615`), `TestGoodQALostResponseReconciles` (`goodqa_test.go:151`) | different scope | Temporal retries activities and relies on the callee's key. Fabric also sends a stable remote key but reads back first because a push or a ticket comment cannot be assumed idempotent. Neither peer page calls the other policy wrong. |
| Atomic record plus dispatch | DISPATCHED commits before transport under an advisory lock held to settlement; `TestBrokerCutpoints` (`broker_test.go:227`), `TestDispatcherSessionLossKeepsOutcome` (`resolve_test.go:868`) | parity | Same intent as the outbox; the remote call cannot join the transaction, hence UNKNOWN. |
| Approval bound to what executes | Hash recomputed before transport; `TestApprovalBindsRequestHash` (`broker_test.go:199`), `TestDispatchRefusesRequestThatNoLongerMatchesItsHash` (`identity_test.go:67`), `TestDispatchRefusesUnapprovedIntentWithApproval` (`resolve_test.go:585`) | parity (peer not documented) | MCP and OWASP recommend human approval but do not describe binding it to a hash. The hash covers the request, not resolved effects (aliases, branch state), policy or tool version. |
| Approval policy | Opt-in per submission; forced only for `publish` (`publish.go:125`); approver is a text field set by the app role, no check that it differs from the requester | behind | OWASP asks approval for high-impact actions by class; Fabric leaves the class decision to the caller. |
| Human settlement audit | Append-only `effect_resolutions`, who/when/transaction stamped by trigger, runner role cannot write; `TestResolveQuarantinedEffect` (`resolve_test.go:39`), `TestAppRoleCannotBypassEffectOutcomes` (`resolve_test.go:135`), `TestEffectTransitionsAllowListed` (`resolve_test.go:515`) | parity (peer not documented) | None of the fetched pages describes an operator exit for an ambiguous effect. The `actor` text is caller-supplied; there is no hash chain and the table owner can disable triggers. |
| Compensation | Not found | behind | A saga undoes completed steps. Fabric stops at QUARANTINED or UNKNOWN and an operator decides; in a mission with two effects where the second fails, the first stays. |
| Least privilege on the effect | Runner role cannot settle outcomes after its run ends (`ErrSettleRunEnded`, migration 0019); `TestAppRoleCannotBypassEffectOutcomes` | parity | Matches OWASP complete mediation: the broker, not the agent, performs the remote call. Credential handling is covered in the credentials note. |
| Tool description and schema pinning | Not found; no MCP client in the repo (search hit only provider launch and stream code) | different scope | The MCP rules apply to an MCP client; Fabric exposes two fixed effect kinds. If MCP tools are added, annotations must be treated as untrusted. |

No `ahead` verdicts: for each Fabric strength the cited peer pages are silent on the point, so it is `parity (peer not documented)` at most.

## Upgrades, ranked
| rank | upgrade | value | effort | gate or risk addressed | ticket: TBD |
|---|---|---|---|---|---|
| 1 | Effect-class policy table (kind, subject pattern) deciding approval, so approval is not a caller option | Caller cannot skip approval for high-impact classes | S | OWASP LLM06 human approval | TBD |
| 2 | Approver must differ from requester and mission creator, enforced by trigger | Two-party control | S | Self-approval | TBD |
| 3 | Add a resolved-effect hash (dry run or computed diff) and policy version to the approval, re-derived at dispatch | Closes stale-consent gap beyond request bytes | M | Approved request differs from what executes | TBD |
| 4 | Declare a compensating effect per kind as a separate approved effect (comment: edit or delete marker; push: revert commit) | Defined cleanup for partial multi-effect missions | M | Partial completion | TBD |
| 5 | Retention and archive for settled effects | Bounded storage | S | Table growth | TBD |

## Not verified
- Whether a relaunched attempt can reuse a business key through another path (`Submit` refuses it; no caller workaround was searched).
- Stripe API v2 behaviour: the page says retries do not repeat successful operations and failed work is retried; not compared.
- Whether the MCP security page is the 2025-06-18 text or a later revision.
- Remote behaviour when GoodQA receives the same operationId with a different body.
