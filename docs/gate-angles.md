# Gate angles: threats a Postgres-backed agent control plane should gate, and where Fabric stands
Status line: pinned to commit bc5a6d3; sources accessed 2026-11-07.

This note lists gate angles beyond the ones Fabric already implements (see review-gates-A.md, R1..R25). For each angle it states the threat, who addresses it today, what Fabric does at bc5a6d3, a design for a Postgres-backed control plane, and a priority. Priorities weigh likelihood times impact for Fabric as it is now: a single-host, shadow-stage system whose workers are coding agents that edit repositories, run shell commands and request effects (ticket comments, publish) through a broker. Source tags: H = primary page read, M = primary abstract or partial read, L = secondary or search summary. L-tagged claims are marked "unverified" and no number from them is relied on. Fabric claims cite file:line at bc5a6d3; where no code was found the text says "not found". Nothing here is a ticket yet.

## Summary

| # | Angle | Threat | Who does it today | Fabric covers | Priority |
|---|---|---|---|---|---|
| 1 | Prompt-injection and taint tracking | Untrusted tool output steers the agent | CaMeL, Meta Rule of Two, Anthropic classifiers (H/M) | no (regex redactor and shell oracle only: policy/redact.go:25-34, policy/oracle.go:32) | P0 Rule-of-Two profile, P2 full flow labels |
| 2 | Independent verifier or judge on effects | Agent's own success claim is wrong | Claude Code auto mode, OpenAI SDK guardrails (H) | no (worker-asserted receipts: reducer.go:626-648) | P0 deterministic checks, P1 model judge |
| 3 | Two-person or quorum approval | One approver or account authorizes destruction | Single-approver gates; IETF draft (L) | partial (hash-bound, single self-approver: broker.go:283, cmd/fab/effect.go:151-155) | P1 (authenticate approver first) |
| 4 | Canary and shadow runs | New model, prompt or loop regresses silently | Anthropic postmortem (H) | partial (recorded fake provider: cmd/fab/job.go:75-79; no dry-effect mode) | P1 |
| 5 | Diff and blast-radius limits | Approved effect is broader than understood | ICML 2026 paper (M); sandboxes (H) | partial (hash recheck, readback; no limits: broker.go:408-411) | P0 limits and protected refs, P1 subsumption |
| 6 | Cross-mission rate and spend breakers | Many missions under cap burn the budget | Agent SDK per-run budget (H) | partial (job and month cap, no rate limit: native.go:34-36) | P0 |
| 7 | Signed provenance and attestation | Cannot prove what produced an output | SLSA, in-toto (H/L) | no (append-only triggers only: 0011_command_bytes.sql:38-44) | P2 |
| 8 | Replay and audit export | Cannot reconstruct why an agent acted | OpenHands event log (H) | partial (pure replay: reducer.go:834; no export) | P1 |
| 9 | TOCTOU on approvals | Executed effect differs from approved | EffectGuard paper (M) | partial (hash and fence rechecks; gap: store/dispatch.go:132-170, sched/sched.go:535-569) | P0 TTL, single-use, target version |
| 10 | Secret egress DLP | Agent sends secrets out | Anthropic sandbox proxy, GitHub push protection (H) | partial (redactor on tool output; egress control not found) | P0 |
| 11 | Supply-chain gates on installed packages | Hallucinated or compromised dependency | Academic measurement (M) | no (not found) | P1 |
| 12 | Model pinning and drift detection | Same alias changes behavior | NIST MEASURE 2.4 (M), postmortem (H) | partial (pin yes: host/grant.go:25-28, agent/loop.go:96; drift no) | pin done; P1 drift probes |
| 13 | Eval gates, held-out sets, Wilson bounds | Regressing change promoted | evals.md | no (no Wilson code found) | P0 minimal suite |
| 14 | Human-review sampling | Rubber-stamp approvals | Anthropic figure (H) | no | P1 |
| 15 | Kill switches, dead-man timers | Runaway agent cannot be stopped | SDK budget stop (H) | partial (lease expiry, capacity 0, park; no dead-man: store/store.go:542, store/reservations.go:99) | P0 halt flag, P1 dead-man |
| 16 | Multi-tenant isolation | Cross-tenant leakage | microVM practice (L) | no (no tenant column found; 0009_reservations.sql:10 defers it) | P1 (P0 once tenants exist) |
| A | Per-mission short-lived credentials | Overbroad token | Amazon Q bulletin (H) | not verified | P0 |
| B | Destructive-operation separation | Data and backups lost together | Incident reports (L) | partial (oracle prod-delete probe: policy/oracle.go:120) | P0 before any destructive kind |
| C | Test and grader tamper detection | Agent edits tests to pass | METR (H) | partial (vacuous receipts rejected: reducer.go:641) | P1 |
| D | Tool-definition pinning | Tool changes after approval | Invariant Labs (L) | partial (tool set in config hash: agent/loop.go:96) | P1 |
| E | Memory and context poisoning | Persisted notes carry injection | OWASP agentic list (L-M) | not verified | P1 |
| F | Cascading-failure bulkheads | One failure fans out | OWASP agentic list (L-M) | partial (capacity gate: cmd/fab/job.go:83) | P1 |
| G | Approver UI shows facts | Agent argues past a human | Claude Code auto mode (H) | not verified | P1 |
| H | Egress allowlist, code-exec containment | Open-ended shell and URL tools | OWASP LLM06 (H) | partial (host sandbox, oracle) | P0 (merged with 10) |
| I | Stuck and rogue agent detection | Loops burn budget | OpenHands (H) | not verified | P1 |
| J | Incident drills | Controls untested | NIST MEASURE (M) | no | P2 |
| K | Refusal handling | Retry loops on refusals | Agent SDK (H) | partial (typed park error: agent/types.go:114) | P2 |

Reference frameworks: OWASP Top 10 for LLM Applications 2025 [genai.owasp.org/llm-top-10](https://genai.owasp.org/llm-top-10/) (accessed 2026-11-07, H); LLM06 Excessive Agency [page](https://genai.owasp.org/llmrisk/llm062025-excessive-agency/) (accessed 2026-11-07, H). The OWASP Top 10 for Agentic Applications 2026 exists at [genai.owasp.org/resource/owasp-top-10-for-agentic-applications-for-2026/](https://genai.owasp.org/resource/owasp-top-10-for-agentic-applications-for-2026/) (accessed 2026-11-07); the item names used for angles D to I came from a third-party page and are unverified. MITRE ATLAS technique IDs and NIST AI 600-1 could not be read from a primary source and are not cited.

## 1. Prompt-injection and tool-output taint tracking
**Threat.** Untrusted text in tool results (web pages, issues, READMEs, repository files) steers the agent (OWASP LLM01, [page](https://genai.owasp.org/llmrisk/llm01-prompt-injection/), accessed 2026-11-07, H). OWASP says mitigations reduce impact and that fool-proof prevention may not exist. Named public incidents (EchoLeak, the GitHub MCP "toxic agent flow") came from secondary sources and are unverified.

**Who does it.**
- CaMeL derives control and data flow from the trusted query and uses capabilities to block exfiltration flows; 77% of AgentDojo tasks solved with provable security against 84% undefended. [arXiv 2503.18813](https://arxiv.org/abs/2503.18813) (accessed 2026-11-07, M, abstract).
- Meta "Agents Rule of Two": one session holds at most two of: processes untrusted input, accesses sensitive data, changes state or communicates externally. [Meta](https://ai.meta.com/blog/practical-ai-agent-security/) (accessed 2026-11-07, H).
- Spotlighting marks input provenance; the authors report injection success falling from over 50% to under 2%. [arXiv 2403.14720](https://arxiv.org/abs/2403.14720) (accessed 2026-11-07, M).
- Anthropic reports 1.4% to 2.4% attack success for browser use with safeguards and says about 1% remains meaningful risk. [Anthropic](https://www.anthropic.com/research/prompt-injection-defenses) (accessed 2026-11-07, H).
- Claude Code auto mode keeps tool outputs away from its action classifier and screens them with a separate probe. [Anthropic](https://anthropic.com/engineering/claude-code-auto-mode) (accessed 2026-11-07, H).

**Fabric today.** No taint tracking. Tool output passes a pattern redactor for credential shapes (policy/redact.go:25-34; TestRedactorShapes, policy/policy_test.go:30). Shell calls pass the oracle guard (policy/oracle.go:32; TestOracleDenyShapes, policy_test.go:161) with four always-on refusals (oracle.go:115-121). Neither records where a tool result came from or whether a later effect was shaped by it.

**Design sketch.**
- Column `trust_class` (operator, repo_owned, third_party, web) on each stored agent turn (`agent_turns`, migration 0022), set by the tool wrapper from tool name and target, not by the model.
- Table `effect_lineage(effect_id, turn_id)` written by the broker at submit from the turns in the current context window (coarse and conservative).
- Table `taint_policy(effect_kind, subject_pattern, max_trust_in_lineage, action)` with action `allow`, `require_approval` or `deny`, read in `Submit` next to the existing `NeedsApproval` flag. A third_party lineage on `publish` or any outbound kind forces approval.
- Table `capability_profile(attempt_id, untrusted_input, sensitive_access, can_act)` with a CHECK that forbids all three true without an approval row. This extends the host grant (host/grant.go:25-28) rather than adding a gate.
- Command `fab effect lineage <id>` prints the contributing turns.

**Limits and risks.** Coarse lineage over-approves (every effect after a web fetch looks tainted) and breeds approval fatigue. Labels are only as good as the wrapper that sets them. Model-level defenses stay probabilistic; strict flow control costs utility.

**Priority.** P0 for the Rule-of-Two profile and a rule on third-party lineage for outbound kinds: workers read untrusted repository and web text and can request publish, and the rule is deterministic. P2 for fine-grained flow labels.

**Ticket:** TBD

## 2. Independent verifier or judge on effects
**Threat.** The agent's claim of success or safety is wrong or inflated. Anthropic lists "marking features done without verification" as a long-running-harness failure ([post](https://www.anthropic.com/engineering/effective-harnesses-for-long-running-agents), accessed 2026-11-07, H). A reported Replit database deletion is from a secondary source and unverified.

**Who does it.**
- Claude Code auto mode runs a two-stage classifier on user messages and tool calls (not outputs): 0.4% false positives and 17% false negatives on 52 real overeager actions; Anthropic says it does not replace human review for high-stakes infrastructure. [Anthropic](https://anthropic.com/engineering/claude-code-auto-mode) (accessed 2026-11-07, H).
- OpenAI Agents SDK tool guardrails wrap function-tool calls but not handoffs or hosted tools; a guardrail error yields "unknown", not a rejection. [docs](https://openai.github.io/openai-agents-python/guardrails/) (accessed 2026-11-07, H).

**Fabric today.** No independent verifier. The reducer checks that each claimed acceptance criterion cites a receipt recorded by this attempt for the current head and that the receipt is not vacuous (reducer.go:626-648), but the receipt content is a worker-asserted string and nothing re-runs it. Effects get readback reconciliation after dispatch (TestPostCommitLossReconcilesToConfirmed, effects/broker_test.go:113), which checks remote state, not whether the effect was a good idea.

**Design sketch.**
- Table `effect_verdicts(effect_id, request_sha, verifier, model_id, prompt_sha, verdict, created_at)`, written only by a verifier DB role that has no UPDATE on `effects`.
- Phase 1 is deterministic and free: schema, path allowlist, size limit, readback match. Phase 2 is a model check from a different family given only the request, the mission spec and the readback, never the agent's narrative.
- The dispatch precondition (broker.go:413-426) adds: for kinds in `effect_policy.requires_verdict`, a `pass` verdict for the same `request_sha` must exist.
- For receipts, a `fab evidence rerun` command re-executes the cited test command in a clean checkout and stores pass/fail counts next to the receipt.

**Limits and risks.** Judges show self-preference and share blind spots with same-family authors (search snippets only; unverified). A verifier that sees only the request cannot judge consent scope, which Anthropic names as its main miss. Re-running tests costs time and needs a trusted checkout.

**Priority.** P0 for deterministic checks and receipt re-run, since receipts are the weakest link in the evidence chain; P1 for the model judge.

**Ticket:** TBD

## 3. Two-person or quorum approval
**Threat.** One approver, or one compromised operator identity, can authorize an irreversible effect. Anthropic reports users accept 93% of manual permission prompts ([post](https://anthropic.com/engineering/claude-code-auto-mode), accessed 2026-11-07, H), so a single prompt is a weak control.

**Who does it.** Claude Code and the OpenAI Agents SDK provide single-approver gates (the SDK pauses and resumes from run state, [docs](https://openai.github.io/openai-agents-python/guardrails/), accessed 2026-11-07, H). An IETF individual draft, EP-QUORUM, describes M-of-N approval by distinct humans; it is not a standard and was read only through a search summary ([draft](https://datatracker.ietf.org/doc/draft-schrock-ep-quorum/), accessed 2026-11-07, L, unverified).

**Fabric today.** Approval is bound to the request hash (broker.go:281-290; TestApprovalBindsRequestHash, effects/broker_test.go:199) but is opt-in per submission: only kind `publish` requires it (publish.go:123-127); `ticket_comment` takes it through `--approve` (cmd/fab/effect.go:132). `approved_by` is a free text column (0003_effects.sql:19) filled from `actor()`, which returns the `FABRIC_ACTOR` environment variable or the string "chat" (cmd/fab/main.go:172-177). `fab effect comment --approve` approves in the same invocation that submits (effect.go:151-155). Approval therefore shows that a byte-identical request was approved by someone able to run the command, not that a second party agreed.

**Design sketch.**
- Table `effect_approvals(effect_id, request_sha, approver, decided_at, decision, PRIMARY KEY (effect_id, approver))`; `effects.approved_by` becomes derived.
- Trigger refusing an approver equal to the submitting actor or the mission creator; table `effect_policy(kind, required_approvals)` (publish and delete = 2).
- `Approve` (broker.go:283) counts distinct approvers on the same `request_sha` and sets `APPROVED` only at the threshold.
- Authentication is the precondition: approver identity must come from a DB role per human or a signed token, otherwise distinctness compares strings the caller chooses. Remove the same-invocation approve path from `effect comment`.

**Limits and risks.** Without authenticated identity quorum is theater. Latency, and availability when approvers are absent (needs TTL, angle 9). Fabric has one operator today, so quorum cannot be met until a second identity exists.

**Priority.** P1 for quorum itself; authenticating `approved_by` is P0 within this angle because the present approval can be forged by any shell user with database access.

**Ticket:** TBD

## 4. Canary and shadow runs before a real effect
**Threat.** A model, prompt or loop change regresses quietly. Anthropic's postmortem shows noisy evals missed three real quality bugs ([post](https://www.anthropic.com/engineering/a-postmortem-of-three-recent-issues), accessed 2026-11-07, H).

**Who does it.** Anthropic's research system uses rainbow deployments so in-flight agents finish on the old version (same engineering blog, H). Offline replay, side-effect-suppressed shadow runs and percentage ramps are described in vendor blogs (L, unverified: [featbit](https://www.featbit.co/blogs/offline-eval-shadow-test-canary-rollout), accessed 2026-11-07); the ramp percentages and hold times there are examples, not norms.

**Fabric today.** The deployment stage is shadow, and the daemon accepts a recorded fake provider (`--provider`, `--trust-provider`, cmd/fab/job.go:75-79), so whole-loop runs against recorded outputs exist. A broker mode that records would-be effects without executing: not found.

**Design sketch.**
- Column `effect_mode` (`live`, `dry`) on `missions`. In `dry` the broker dispatch writes the request and a synthetic resolution with `executed=false` and skips `exec`. Table `shadow_diff(mission_id, shadow_attempt, primary_attempt, request_sha_a, request_sha_b)` compares planned effects.
- Table `agent_configs(config_sha, model, prompt_sha, tool_sha, loop_version, mode, rollout_pct)`; assignment by hash of the mission id; in-flight attempts keep the config recorded in their grant (host/grant.go:25-28).

**Limits and risks.** Suppressed effects change downstream behavior, so shadow diverges from live. Doubles spend. At low volume there is little statistical power.

**Priority.** P1: it needs the eval suite of angle 13 first; the immediate need is only the dry mode.

**Ticket:** TBD

## 5. Diff and blast-radius limits per effect
**Threat.** The approver consents to a small effect and a larger one executes. An ICML 2026 paper reports that tool-, session- and argument-level approval committed unsafe broadened effects in 7.8% of 1,377 real-agent proposals, while a dry-run, resolved-effect design committed none ([abstract](https://icml.cc/virtual/2026/67824), accessed 2026-11-07, M, abstract only).

**Who does it.** Sandboxes restrict filesystem and network ([Anthropic](https://www.anthropic.com/engineering/claude-code-sandboxing), accessed 2026-11-07, H). OWASP LLM06 recommends least privilege, rate limiting and avoiding open-ended tools (H). No peer was found that enforces per-effect row or file count limits generically.

**Fabric today.** The broker recomputes the request hash before transport (broker.go:408-411; TestDispatchRefusesRequestThatNoLongerMatchesItsHash, effects/identity_test.go:67) and runs readback reconciliation. Size or count limits, protected resource lists, and a check that the readback fell within the predicted effect: not found. The publish executor pushes a git ref (publish.go); the request, not a size policy, bounds what it pushes.

**Design sketch.**
- Table `effect_limits(kind, max_files, max_bytes, max_resources, protected_refs text[])`; columns `resolved_effect jsonb` and `resolved_sha` on `effects`, filled by an executor `Resolve` step (dry-run output; for git, the changed path list and the target ref's current sha).
- Approval binds `resolved_sha` as well as `request_sha`; the broker recomputes `Resolve` just before exec and refuses unless the new effect is a subset of the approved one.
- Over-limit effects force angle 3 quorum. Add a post-readback check "observed change within resolved_effect".

**Limits and risks.** Needs executor-specific dry-run support; many remote APIs have none. Tight limits block legitimate large migrations, so each needs an owner-reviewed override.

**Priority.** P0 for protected refs and count limits on publish (cheap, deterministic); P1 for the subset recheck.

**Ticket:** TBD

## 6. Cross-mission rate and spend circuit breakers
**Threat.** Many missions, each under its own cap, together exhaust the budget (OWASP LLM10 Unbounded Consumption, listed on the top-10 page above, H).

**Who does it.** The Agent SDK has a per-run `max_budget_usd` with documented one-response overshoot (H). Community libraries for pre-call cost estimates and breakers exist (L, unverified).

**Fabric today.** Native caps: USD 5 per job, USD 20 per month, USD 1 per live test, enforced in code and not raisable by a grant (native/native.go:34-36; TestGrantMonthCeiling, native/cap_test.go:21; TestMonthCeilingStopsLoop, native/month_test.go:54). A budget shortfall before launch parks the mission (TestPrelaunchBudgetParksMission, sched/budget_test.go:12). No rolling-window rate limit, no breaker on error ratio or repeated identical tool calls, and no cross-mission trip other than the monthly cap.

**Design sketch.**
- Tables `breakers(scope, state closed|open|half_open, opened_at, reason, opened_by)` and `window_counters(scope, window_start, spend_micros, calls, errors)`, updated with `INSERT ... ON CONFLICT DO UPDATE` in the ledger transaction that already records spend (agent/ledger.go).
- Pre-lease check in the scheduler: no new attempt while the global, effect-class or tool breaker is open; half-open admits one probe attempt.
- Trip conditions: spend per minute above N, an identical tool call repeated K times, effect denial rate.

**Limits and risks.** Counters overshoot by one call. A wrong threshold causes an outage, so each trip needs an operator-readable reason and a one-command reset.

**Priority.** P0: the month cap is the only fleet-wide stop, a retry loop could consume it within hours, and the change is small.

**Ticket:** TBD

## 7. Signed provenance and attestation
**Threat.** A reader cannot tell which model, prompt and tools produced a commit. The Amazon Q extension incident (an overbroad token let an attacker push a data-wiping prompt into release 1.84.0) shows the supply-chain side ([bulletin](https://aws.amazon.com/security/security-bulletins/AWS-2025-015/), accessed 2026-11-07, H).

**Who does it.** SLSA's Build track gives tamper-resistant build evidence and says nothing about code quality ([SLSA v1.1](https://slsa.dev/spec/v1.1/about), accessed 2026-11-07, H; this is a retired version, v1.2 is current). In-toto and Sigstore sign general artifacts; no standard predicate for agent runs was found.

**Fabric today.** No signing. `events` and `commands` reject UPDATE, DELETE and TRUNCATE by trigger (0011_command_bytes.sql:38-44), as does `effect_resolutions` (0020_effect_resolutions.sql:46,186). There is no hash chain, and the table owner can disable the triggers. The grant records model and tier-table digest (host/grant.go:25-28).

**Design sketch.** After each completed attempt, emit an in-toto-style JSON statement (attempt id, config sha, base commit, result commit, effect request hashes, verdict ids), sign it with a key held outside the application role, and store it in `attestations(id, prev_hash, body, signature)` with a hash chain. Verify at publish time.

**Limits and risks.** It attests process, not correctness; a compromised control plane signs lies; key custody is a new operational burden.

**Priority.** P2: at shadow stage nothing downstream consumes attestations. Raise to P1 when an outside party needs audit evidence.

**Ticket:** TBD

## 8. Reproducible replay and audit export
**Threat.** The operator cannot reconstruct why an agent acted, or hand an auditor a self-contained record.

**Who does it.** OpenHands persists an event log and records condensations so views can be rebuilt ([docs](https://docs.openhands.dev/sdk/arch/condenser), accessed 2026-11-07, H). OpenAI traces have a sensitive-data toggle (H). NIST AI RMF MEASURE 2.8 calls for audit trails (M).

**Fabric today.** The reducer is pure and replays the event log into mission state, stopping at unknown event kinds (reducer.go:829-838). Rebuild is tested for one snapshot under a concurrent writer, corruption detection and rejection of unknown events (TestRebuildConcurrentWriterUsesOneSnapshot, TestVerifiedSnapshotIsReadOnlyAndDetectsCorruption, TestVerifiedSnapshotRejectsUnknownEvent, store/rebuild_test.go:18,48,74). Agent turns are persisted (migration 0022). An audit export command and a signed bundle: not found.

**Design sketch.** `fab mission export <id>` writes NDJSON of commands, events, effect rows, resolutions and redacted turns, with a manifest of SHA-256 per file; `--verify` replays the events and compares final state. A replay mode of the native loop feeds recorded model outputs and tool results to exercise the loop and effect policy offline as a regression test.

**Limits and risks.** Model calls are not deterministic, so replay means replaying recorded outputs. Transcripts hold secrets, so export must redact and be operator-only. Storage grows.

**Priority.** P1: the pieces exist, export is small, and it helps every incident.

**Ticket:** TBD

## 9. Time-of-check vs time-of-use on approvals
**Threat.** The effect that executes differs from the one approved. The ICML paper above (M) quantifies it for resolved effects. Framework forum reports of mutated arguments and double execution on resume are single-source L reports and unverified.

**Who does it.** EffectGuard-style revalidation before execution (M, same abstract). Hash-bound approval tokens appear in some libraries (L, unverified).

**Fabric today.** Strong at the argument level: dispatch recomputes the request hash before transport and refuses `ErrRequestTampered` (broker.go:408-411; effects/identity_test.go:67), refuses an effect that carries an approval but is still `INTENT` (broker.go:414-419), requires `approval_sha == request_sha` (broker.go:421-423), and rechecks the fence after the precondition round trip (broker.go:456-458). Gaps: final admission and process start are separate steps (`CheckDispatchFresh`, store/dispatch.go:132-170; the scheduler admits, then creates the stream file and starts the runner, sched/sched.go:535-569), so a lease can lapse in between; approvals have no TTL and no explicit single-use field (not found); the hash does not cover resolved effects such as a branch tip moving between approval and push.

**Design sketch.** Add `policy_version`, `tool_sha`, `target_version` (for git, the remote ref's sha at approval) and `expires_at` to the approval row. The dispatch statement becomes `UPDATE effects SET state='DISPATCHED' WHERE id=$1 AND state='APPROVED' AND approval_sha=request_sha AND target_version=$2 AND expires_at>now()`. Make the admission-to-exec gap harmless by having the runner re-check its fence at start.

**Limits and risks.** A TTL that is too short strands approvals; expiry must use the database clock (`now()` in SQL), not the application clock.

**Priority.** P0 for TTL, single-use and target version on publish: small cost, and publish is the only effect kind that is always approved.

**Ticket:** TBD

## 10. Secret egress DLP
**Threat.** Agents read environment variables and files and send them out through network calls, git pushes, issue comments or PR bodies. The Nx "s1ngularity" incident (malicious packages invoking installed AI CLIs to inventory secrets) appears only in secondary vendor posts (L, unverified; counts differ by source).

**Who does it.**
- Anthropic's sandbox uses a proxy outside the sandbox with domain rules and states that filesystem and network isolation are both required. [Anthropic](https://www.anthropic.com/engineering/claude-code-sandboxing) (accessed 2026-11-07, H).
- GitHub push protection blocks known secret patterns on CLI, UI, REST and some MCP pushes; any writer can bypass with a reason unless delegated bypass is configured; AI-detected generic secrets are in public preview. [docs](https://docs.github.com/en/code-security/secret-scanning/introduction/about-push-protection) (accessed 2026-11-07, H).

**Fabric today.** Regex redaction of credential shapes in tool output (policy/redact.go:25-34; TestRedactorShapes, policy_test.go:30) and an oracle refusal of credential-file dumps (policy/oracle.go:117). Host sandbox profiles exist for Linux (host/landlock_linux.go) and macOS (host/darwin.go); whether egress is default-deny: not verified. Scanning outbound effect payloads (push content, comment body) and canary tokens: not found.

**Design sketch.**
- Table `egress_allow(mission_id, host_pattern)`; default deny at the sandbox network layer, with credentials injected by a proxy per request so workers hold no long-lived secrets.
- Seed each sandbox environment with canary strings (`canaries(mission_id, sha)`); the broker scans every outbound effect request and the proxy log for a canary, and on a hit opens a breaker (angle 6) and writes an incident row.
- Add a payload scan step to `publish` Validate (publish.go:123-131) using the redactor's pattern set.

**Limits and risks.** Pattern scanners miss unstructured or encoded secrets; allowed hosts can serve as drop boxes; canaries only catch what the agent could read.

**Priority.** P0 for canaries plus an egress allowlist (cheap, catches real exfiltration); P1 for payload scanning.

**Ticket:** TBD

## 11. Dependency and supply-chain gates on agent-installed packages
**Threat.** Models invent package names that attackers can register. Spracklen et al. tested 16 models over 576,000 samples: hallucinated packages averaged at least 5.2% for commercial and 21.7% for open-source models, with 205,474 unique invented names ([arXiv 2406.10279](https://arxiv.org/abs/2406.10279), accessed 2026-11-07, M, abstract). Repeatability figures from news summaries are unverified.

**Who does it.** Scanners and package-age policies exist in vendor products (L, not read, unverified). SLSA does not yet cover dependency integrity (H, link in angle 7).

**Fabric today.** Not found. The oracle's four refusals do not cover package installs (policy/oracle.go:115-121).

**Design sketch.** Treat `npm install`, `pip install` and `go get` as effect class `dependency_add`: the oracle routes it to a broker check against an allowlist or internal mirror, a minimum package age and a lockfile with hashes, and runs installs with scripts disabled and no secrets. Record name, version and hash in the attempt's provenance row. Failing control: a nonexistent-package fixture must be rejected.

**Limits and risks.** Existence is not safety; typosquats of real packages pass an existence check; a mirror is an operations burden.

**Priority.** P1: coding agents install packages routinely, but the mitigation leans on the egress control of angle 10.

**Ticket:** TBD

## 12. Model and version pinning, drift detection
**Threat.** The same alias changes behavior over time (GPT-3.5/4 accuracy shifts within three months, [arXiv 2307.09009](https://arxiv.org/abs/2307.09009), accessed 2026-11-07, M). Anthropic's August-September 2025 infrastructure bugs degraded quality for weeks ([postmortem](https://www.anthropic.com/engineering/a-postmortem-of-three-recent-issues), accessed 2026-11-07, H).

**Who does it.** Anthropic recommends continuous production quality evals after the incident (H). NIST MEASURE 2.4 compares production behavior with pre-deployment results (M).

**Fabric today.** Pinning: the host grant carries model, rung and tier-table digest and is rejected when empty (host/grant.go:25-28, 54-61), and the native loop refuses a resume under a different model, system prompt, tool set, price table or extra parameters (agent/loop.go:96-100). Drift detection (probe set, control chart): not found.

**Design sketch.** Store the exact model id and `config_sha` per attempt; refuse a lease for a config not marked active in `agent_configs`; run a small deterministic probe set on a timer and alert when the Wilson lower bound of the pass rate falls below a floor, opening a breaker (angle 6).

**Limits and risks.** Pinning does not protect against provider infrastructure bugs; snapshots retire; probes cost money and are noisy at small n.

**Priority.** Recording and pinning are already in place. P1 for drift probes: at shadow stage volume is low and the probe set depends on angle 13.

**Ticket:** TBD

## 13. Eval gates with held-out sets and Wilson bounds before promotion
**Threat.** A model, prompt or tool-schema change is promoted without evidence it is no worse. Sources and method are in the evals research note, not repeated here.

**Who does it.** See evals.md in the research set.

**Fabric today.** No Wilson bound or paired-test code found (searched `internal` and `cmd` for "wilson"). Live tests and per-criterion receipts exist but are not a held-out promotion suite.

**Design sketch.** Table `eval_runs(config_sha, suite, n, passes, wilson_lower, created_at)`; `agent_configs.mode` may move to `active` only if a row for the same `config_sha` has `wilson_lower` above the suite floor, enforced by a trigger. Include a small safety sub-suite (injection, destructive-command refusals) with must-fail controls.

**Limits and risks.** Small suites give wide bounds; held-out sets leak when reused; thresholds must be fixed before running.

**Priority.** P0 for a minimal golden suite: the promotion gates in angles 4 and 12 depend on it.

**Ticket:** TBD

## 14. Human-review sampling
**Threat.** Approvers rubber-stamp: 93% manual approval rate per Anthropic ([post](https://anthropic.com/engineering/claude-code-auto-mode), accessed 2026-11-07, H). Human miss rates quoted by a blog are unverified.

**Who does it.** Practitioner advice (L, unverified) is to sample by risk stratum, seed known-bad items and lower the rate for strata with zero findings. Anthropic's classifier escalates after repeated denials (H).

**Fabric today.** No sampling; approvals are manual and only on publish and opted-in comments (publish.go:123-127; effect.go:132).

**Design sketch.** `review_samples(effect_id, stratum, sampled, seeded, reviewer, verdict, decided_at)`; deterministic sampler `hash(effect_id) mod 100 < rate(stratum)`, 100% for the first N effects of a new config or kind; seeded known-bad effects (broker stub, never executed) measure per-operator catch rate; alert when approvals are near 100% with sub-second decisions.

**Limits and risks.** Sampling misses rare failures; seeded items must look real.

**Priority.** P1: low volume means review is complete today; it matters once autonomy grows.

**Ticket:** TBD

## 15. Kill switches and dead-man timers
**Threat.** A runaway agent cannot be stopped, or an absent operator leaves long attempts running.

**Who does it.** The Agent SDK stops on max turns or budget and OpenAI offers `cancel(mode="after_turn")` (H, docs linked in angle 2). Guidance on shared-store kill switches and watchdogs is from community posts (L, unverified).

**Fabric today.** Lease expiry revokes an attempt (lease updates, store/store.go:542,573; sweep via `fab expire-leases`, cmd/fab/main.go:51; TestRV2_SweepReachesEveryExpiredLease, store/review_test.go:76). The daemon's `--hold` flag turns on the dispatch gate (cmd/fab/job.go:83); capacities are set with `fab db capacity` (cmd/fab/db.go:68), and `SetCapacity` accepts 0 (store/reservations.go:99-102), so setting a resource to 0 should stop new grants (inferred from code; not run). Parking and `unpause` are operator commands (cmd/fab/main.go:39,248; TestParkedAdmitRefusedForAutomationActors, store/park_actor_test.go:14). A global halt flag checked before every model call, and a dead-man timer requiring periodic operator acknowledgement: not found.

**Design sketch.** Table `halt(scope global|mission, set_by, reason, set_at)` checked at lease renewal, before each model call and in the broker dispatch precondition; fencing already refuses stale holders. Dead-man: for missions above a tier, an `operator_ack_due` timestamp; when it passes the scheduler parks (pause, not kill) so partial effects reconcile. Command `fab halt set|clear`.

**Limits and risks.** Stopping mid-effect leaves partial state, so a halt must route through broker reconciliation. A halt flag in the same database is unavailable when the database is.

**Priority.** P0 for the halt flag (small; today the only stops are capacity 0 or killing processes); P1 for the dead-man acknowledgement.

**Ticket:** TBD

## 16. Multi-tenant isolation
**Threat.** Cross-tenant leakage through shared kernel, DB roles, caches and memory. The Claude API isolates prompt caches by organization and workspace (caching docs, H).

**Who does it.** Vendor blogs favor microVMs (Firecracker) or gVisor over shared-kernel containers for hostile code (L, unverified). OWASP LLM06 advises executing in the user's context with least privilege (H).

**Fabric today.** One database with separated app and runner roles (store/roles.go, effects/runner_role_test.go; not re-read line by line for this note). No tenant column found; the reservations migration lists "tenant-first keys" as deferred (0009_reservations.sql:10). Linux hosts can use a dedicated worker account, otherwise workers run same-UID in shadow mode (cmd/fab/job.go:80-81).

**Design sketch.** Add `tenant_id` to missions and every child table, row-level security keyed on a per-connection `fabric.tenant` setting, one DB role per component, tenant id inside fence checks, per-tenant sandbox network and credentials. Cross-tenant tests as must-fail mutants (drop a policy, expect a failing test).

**Limits and risks.** Retrofitting a tenant column into append-only tables is a migration risk; microVM isolation raises cost.

**Priority.** P1: there is one operator now. It becomes P0 when a second tenant is planned, and the schema change is cheaper before more tables exist.

**Ticket:** TBD

## Additional angles from the research set

**A. Per-mission short-lived credentials.** Threat: an overbroad token turns a small mistake into an incident (Amazon Q bulletin linked in angle 7, H; OWASP LLM06 excessive permissions, H). Fabric: how workers obtain git and API credentials was not verified. Sketch: the broker mints a per-attempt credential with TTL equal to the lease and revokes it on halt (`credential_grants`). Priority P0, because the other controls assume the worker cannot reuse a long-lived token. Ticket: TBD.

**B. Destructive-operation separation and recoverability.** Threat: production data and its backups lost together (incident reports, L, unverified). Fabric: the oracle denies a production-delete shape (policy/oracle.go:120), but the effect row has no restore-point requirement. Sketch: destructive effect classes need quorum (angle 3), a verified restore point id in the effect row, and production credentials the sandbox never holds. Priority P0 before any destructive kind is added; Fabric has none today. Ticket: TBD.

**C. Test and grader tamper detection.** Threat: the agent edits tests or graders to pass (METR reports reward hacking, H; ImpossibleBench, L, unverified). Fabric: vacuous receipts (passed = failed = 0) are rejected (reducer.go:641-643); a diff policy forbidding edits to test files, CI configuration and graders: not found. Sketch: path policy on the candidate diff plus a rerun from a trusted checkout of the tests. Priority P1. Ticket: TBD.

**D. Tool-definition pinning and rug-pull detection.** Threat: a tool's description changes after approval (Invariant Labs, L, unverified). Fabric: the native loop's config hash covers the tool set (agent/loop.go:96-100), so a resume under a changed tool set is refused; use of external tool servers was not verified. Sketch: store `tool_sha` with each approval, refuse on mismatch, and treat tool descriptions as untrusted in angle 1. Priority P1. Ticket: TBD.

**E. Memory and context poisoning controls.** Threat: persisted notes carry injected text into later missions (OWASP agentic list, names unverified). Fabric: persistent agent memory was not verified in code. Sketch: provenance and trust class on any persisted note, quarantine of notes written from third-party lineage, summaries inheriting the highest taint. Priority P1. Ticket: TBD.

**F. Cascading-failure containment.** Threat: a failure fans out through retries. Fabric: the dispatch gate and capacity table cap concurrent holds (cmd/fab/job.go:83; store/reservations.go:99); a dispatch error parks only that mission (TestDispatchErrorParksOnlyThatMission, sched/isolation_test.go:60). Sketch: per-class slot caps, no cross-mission retry fan-out, breakers from angle 6. Priority P1. Ticket: TBD.

**G. Approver UI shows facts, not narrative.** Threat: an agent argues a human into approving; Claude Code's classifier is blind to assistant text for this reason (H, auto-mode post). Fabric: the approval view was not verified. Sketch: show the resolved effect, diff and lineage first, and the agent's rationale collapsed and labeled. Priority P1. Ticket: TBD.

**H. Egress allowlisting and code-execution containment.** Threat: open-ended shell and URL tools (OWASP LLM06 advises narrow tools, H). Fabric: host sandboxes and the shell oracle contain execution (host/landlock_linux.go, policy/oracle.go:32); default-deny egress was not verified. Sketch: see angle 10. Priority P0, merged with angle 10. Ticket: TBD.

**I. Stuck and rogue agent detection.** Threat: loops that burn budget; OpenHands has stuck-pattern detection (H, per the native-loop note). Fabric: loop-guard behavior was not verified here. Sketch: an identical-call repeat counter feeding the breaker of angle 6. Priority P1. Ticket: TBD.

**J. Incident drills.** Threat: controls untested when needed (NIST MEASURE 2.6 and 2.8, M). Fabric: none found. Sketch: scripted kill, revoke and restore drills with recorded evidence. Priority P2. Ticket: TBD.

**K. Refusal and policy-block handling.** Threat: retry loops on provider refusals; the Agent SDK exposes `stop_reason == "refusal"` (H). Fabric: the native loop has a typed park error (agent/types.go:114; provider/anthropic/stream.go:431), but treatment of a refusal as a terminal, operator-visible state was not verified. Sketch: map refusal to parked-with-reason. Priority P2. Ticket: TBD.

## Not verified
- Whether sandboxes enforce default-deny egress, and how workers obtain git credentials (angles 10, A, H).
- Whether `fab db capacity` set to 0 halts dispatch end to end; the code accepts 0 but no run was made.
- Line numbers in store/roles.go and the exact runner-role tests.
- Loop-guard behavior (I), persistent memory (E) and the approval view (G).
- All L-tagged incident reports (EchoLeak, GitHub MCP flow, Replit, PocketOS, s1ngularity, forum approval reports) and the IETF draft; MITRE ATLAS and NIST AI 600-1 primaries were unreadable.
