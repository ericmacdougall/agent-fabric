# Operator UX, approvals and audit: state of the art and where Fabric stands
Status line: pinned to commit bc5a6d3; sources accessed 2026-11-07.

## What this component does in Fabric
- Operator actions (re-admit a parked mission, unpause, resolve a quarantined effect) require an actor named `op:<name>` taken from `FABRIC_ACTOR` and a reason (reducer.go:99-101, 123-129; broker.go:797-803; cmd/fab/main.go:172-177). The code states that actors are self-asserted and separate code paths, not authenticated people (internal/domain/ids.go:62-71).
- Approval is a flag: `fab effect comment --approve` submits the effect, and when it is `ApprovalRequired` calls `Approve` in the same process with `actor()` as approver (cmd/fab/effect.go:149-155; the flag is at effect.go:132). `Approve` updates only when the stored `request_sha` equals the cited hash (internal/effects/broker.go:283-285) and records `approved_by`.
- The command and event history rejects UPDATE, DELETE and TRUNCATE through triggers (0011_command_bytes.sql:38-44); `effect_resolutions` is append-only (0020_effect_resolutions.sql:44-46) and effects cannot be deleted (0020:65). There is no hash chain, no separate audit-writer role, and the table owner can disable triggers.
- `fab tui` is read-only: its pool is read-only by default (internal/tui/load.go:97-98). `fab metrics` and `fab serve-metrics` expose Prometheus text, and the HTTP listener must be a loopback IP unless `--allow-remote` is passed (internal/metrics/http.go:18-35).
- Database roles: the runner role cannot approve effects or raise the slot cap (`TestRunnerCannotApproveEffects`, roles_test.go:173; `TestRolesLeastPrivilege`, roles_test.go:51, 75). Only actor `fabd` may mark a pre-launch or capacity-wait finish (store/asks.go:43).

## State of the art
- Deterministic rule evaluation, not model judgment: deny, then ask, then allow; first match wins; an allow cannot carve an exception from a deny; the client enforces rules. Claude Code, [permissions](https://code.claude.com/docs/en/permissions) (accessed 2026-11-07), H.
- Rules are not a boundary alone: Bash rules match command text and miss other invocation forms, and the docs point to OS sandboxing for filesystem and network enforcement. Same page, H.
- Administrators can disable risky modes in managed settings so users cannot override them (`disableBypassPermissionsMode`). Same page, H.
- Non-interactive approval routing: `--permission-prompt-tool` delegates prompts to an MCP tool, and denying when nobody can answer is the unattended default. Claude Code, [CLI reference](https://code.claude.com/docs/en/cli-reference) (accessed 2026-11-07), H.
- Checkpointed human-in-the-loop: `interrupt()` persists state and resumes on the same thread id; the node restarts from its beginning on resume, so side effects before the interrupt must be idempotent. LangGraph, [interrupts](https://docs.langchain.com/oss/python/langgraph/interrupts) (accessed 2026-11-07), H.
- Risk-tiered confirmation: LOW, MEDIUM, HIGH and UNKNOWN risk mapped to a confirmation policy. OpenHands SDK, [security API](https://docs.openhands.dev/sdk/api-reference/openhands.sdk.security) (accessed 2026-11-07), M (search summary, page not fetched). The default analyzer reads a risk value supplied by the model, which is weak against an adversarial model (inference).
- Breaker operator controls: manual reset or force-open, and an event on each state change. Azure Architecture Center, [circuit breaker](https://learn.microsoft.com/en-us/azure/architecture/patterns/circuit-breaker) (accessed 2026-11-07), H.
- Headroom visibility for operators: usage page with rate-limit headroom and cache hit rate. Anthropic, [rate limits](https://platform.claude.com/docs/en/api/rate-limits) (accessed 2026-11-07), H.

## Comparison
| Practice | Fabric today (file:line and test) | Verdict | Why |
|---|---|---|---|
| Deterministic gate on risky actions, not model judgment | Reducer checks operator and reason in code (reducer.go:99-129); `TestParkedAdmitIsOperatorOnly` (park_test.go:101), `TestUnpauseIsOperatorOnly` (unpause_test.go:12), `TestParkedAdmitRefusedForAutomationActors` (park_actor_test.go:14) | parity | Same principle as Claude Code's rule engine: code decides, not the model. |
| Authenticated operator identity | No. Actor is an environment variable (main.go:172-177); `IsOperator` is a prefix test (ids.go:71) | behind | Claude Code managed settings are set by an administrator and cannot be overridden by the user. In Fabric any local user with database access can write any actor, so audit rows prove attribution claims, not identity. This is the largest gap. |
| Separation of duty on approval | No. `effect comment --approve` approves its own submission (effect.go:149-155); `Approve` checks hash only (broker.go:283) | behind | A single operator is both requester and approver. The runner role cannot approve (`TestRunnerCannotApproveEffects`), so an agent cannot self-approve through its own database role. |
| Approval bound to the exact request | `TestApprovalBindsRequestHash` (broker_test.go:199): wrong hash gives `ErrApprovalMismatch`; unapproved dispatch refused (`TestDispatchRefusesUnapprovedIntentWithApproval`, resolve_test.go:585) | parity | LangGraph resumes by thread id with no hash binding documented; Fabric binds to content. The peer's page is silent on tamper checks, so not `ahead`. |
| Approval expiry with default deny | Not found | behind | No TTL on `ApprovalRequired`; an old approval request stays approvable. |
| Risk-tiered approval policy as data | Not found; `NeedsApproval` is a per-call flag (effect.go:132, 144) | behind | OpenHands maps risk to policy. Fabric's tier is whoever sets the flag. |
| Safe-by-default unattended run | Operator-only paths refuse automation actors; park holds until a human re-admits | parity | Matches deny-when-unanswered. |
| Append-only history | Trigger-enforced for commands, events, `effect_resolutions` (0011, 0020); `TestCommandRowChecks` (command_bytes_test.go:38) attempts UPDATE, DELETE and TRUNCATE | different scope | The peers cited do not document audit storage. Fabric's rejection is not tamper-evident: no hash chain and the owner can drop triggers. |
| Operator write surface | CLI commands with `--key` and `--reason`; TUI is read-only (tui/load.go:97-98; `TestReadOnlyPool`, tui_test.go:625) | different scope | Read-only is deliberate. There is no confirm view showing the request hash and normalized effect before an approval. |
| Operator visibility of queue, spend, asks | TUI, `/metrics` with open asks and oldest-ask age (metrics.go:244-252); `TestGoldenFrame` (tui_test.go:173), `TestNoSecretsInFrames` (tui_test.go:647), `TestHostileTextInert` (tui_test.go:426) | parity | Anthropic's console shows rate-limit headroom; Fabric shows spend and caps but not pending-approval age. |
| Metrics endpoint exposure | Loopback only without `--allow-remote`; `TestCheckListen` (metrics_test.go:247) | parity (peer not documented) | No authentication on the endpoint when remote is allowed. |

## Upgrades, ranked
| rank | upgrade | value | effort | gate or risk addressed | ticket |
|---|---|---|---|---|---|
| 1 | Per-operator database role or token, with `FABRIC_ACTOR` derived from it | High | Med | Forged actor on audit rows | TBD |
| 2 | Requester not equal to approver for effect settle and approval, with a negative test | Med | Low | Self-approval | TBD |
| 3 | Approval TTL with default deny on expiry, plus a pending-approval age metric | Med-High | Low | Stale approvals, unattended runs | TBD |
| 4 | Hash chain over operator audit rows and a verify command | Med | Low-Med | Tamper evidence beyond triggers | TBD |
| 5 | Command that prints request hash and normalized effect before an approval is recorded | Med | Low | Approving the wrong request | TBD |
| 6 | Risk-tier table for effect classes driving the approval requirement | Med | Med | Inconsistent approval use | TBD |

## Not verified
- Correction to the research note: the audit tables are trigger-protected, not only by convention (0011, 0020); the 0011 triggers are exercised by `TestCommandRowChecks` and `TestCommandChecksHoldWithTriggersOff` (command_bytes_test.go:38, 80), the exact assertions were not re-read for `effect_resolutions`.
- Whether `Approve` is reachable by any role other than the scheduler or owner role beyond what `TestRolesLeastPrivilege` covers; only `TestRunnerCannotApproveEffects` was cited.
- The TUI and metrics lines were read at the cited positions, but their full flag set was not.
- OpenHands details come from a search summary, not the page.
