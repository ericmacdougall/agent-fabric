# Observability and audit: state of the art and where Fabric stands
Status line: pinned to commit bc5a6d3; sources accessed 2026-11-07.

## What this component does in Fabric
- The record of what happened is the database: `events`, `commands` and `effect_resolutions` are append-only by trigger. Row triggers reject UPDATE and DELETE and statement triggers reject TRUNCATE for `commands` and `events` (`internal/store/migrations/0011_command_bytes.sql:38-45`, function from `0009`), and for `effect_resolutions` (`0020_effect_resolutions.sql:46-47,186-187`). An insert trigger stamps `db_user` (`current_user`), transaction id and time, so a caller cannot choose them (`0020:30-39`).
- Effect outcomes go through a guard trigger that blocks deletes, illegal state moves and any `resolved`/`abandoned` entry without an audited resolution row (`0020:61-148`).
- `fab serve-metrics` exposes Prometheus text from one read-only repeatable-read transaction (`internal/metrics/metrics.go:100-119`). It listens on loopback by default, has no auth and no write route, and `--allow-remote` lifts the loopback check (`internal/metrics/http.go:22-37`, `cmd/fab/metrics.go:85-88`). `fab tui` is a read-only terminal view (`cmd/fab/tui.go:68`).
- Native loop turns are persisted in `agent_messages`/`agent_tool_calls` with per-turn request ids, usage and cost (`0022_agent_turns.sql`).

## State of the art
- OpenTelemetry GenAI semantic conventions (now in their own repository) define agent spans (`create_agent`, `invoke_agent`, `invoke_workflow`), tool-execution spans and attributes such as `gen_ai.operation.name` and `gen_ai.conversation.id`. The agent-span document has status Development. Message, instruction and tool-definition content is opt-in and flagged as likely personal data. [semantic-conventions-genai](https://github.com/open-telemetry/semantic-conventions-genai/blob/main/docs/gen-ai/gen-ai-agent-spans.md) (accessed 2026-11-07), H.
- Claude Code ships OpenTelemetry metrics, logs and (beta) traces over OTLP. Span tree: interaction, llm_request, tool, tool.execution. Prompt and tool content is redacted by default with per-category opt-in; Bash subprocesses inherit `TRACEPARENT` but not `OTEL_*`; repository settings cannot redirect telemetry and managed settings can pin the endpoint. [Claude Code monitoring](https://code.claude.com/docs/en/monitoring-usage) (accessed 2026-11-07), H (first 100k of 173k characters).
- Langfuse is an open-source, self-hostable tracing product with nested observations and asynchronous batched ingestion; the page read did not mention OTel. [Langfuse](https://langfuse.com/docs/observability/overview) (accessed 2026-11-07), M.
- Transparency logs: Sigstore Rekor is an append-only ledger whose append-only property auditors can verify. Rekor v2 (GA 2025-10-10) is tile-based with inclusion proofs on upload. [Rekor docs](https://docs.sigstore.dev/logging/overview/) (accessed 2026-11-07), H; [Rekor v2](https://blog.sigstore.dev/rekor-v2-ga/) (accessed 2026-11-07), H.
- NIST SP 800-53 AU-9: AU-9(2) keep audit records on a physically different system, AU-9(3) cryptographic protection of audit information, AU-9(4) restrict audit management to a subset of privileged users. [AU-9 text (third-party mirror)](https://csf.tools/reference/nist-sp-800-53/r5/au/au-9/) (accessed 2026-11-07), M.
- pgAudit gives session and object audit logging through the Postgres log for compliance audits. [pgAudit](https://www.pgaudit.org/) (accessed 2026-11-07), M.

## Comparison
| Practice | Fabric today (file:line and test) | Verdict | Why |
|---|---|---|---|
| Tamper-evident audit store | Triggers reject UPDATE/DELETE/TRUNCATE (`0011:38-45`, `0020:46-47,186-187`); tests `TestCommandRowChecks` (`internal/store/command_bytes_test.go:66`, TRUNCATE and forged rows), `TestResolveQuarantinedEffect` (`internal/effects/resolve_test.go:100`, UPDATE and DELETE), `TestAppRoleCannotBypassEffectOutcomes` (`resolve_test.go:188`) | behind | Rekor and AU-9(3) give verifiable append-only. Triggers stop the app and runner roles but not the table owner, who can disable them (standard Postgres behavior; not tested here). No hash chain, no signature, no external anchor. The research notes said "append-only by convention"; the code enforces it by trigger, but only at the database layer. |
| Actor attribution | `db_user`, `xact`, `at` stamped by trigger (`0020:30-39`); app, runner and scheduler roles exist (`roles.go:81`) | parity (peer not documented) | Attribution is by database role. All runner workers share one role, so it names the class, not the run (see credentials). |
| Separate audit-writer role | No: the `app` role holds DML on every table and relies on triggers (`roles.go:95`); tests `TestRolesLeastPrivilege` (`roles_test.go:51`), `TestRunnerCannotShadowGuardTables` (`roles_test.go:256`) | behind | AU-9(4) asks to restrict audit management to few users. Fabric restricts the runner role well; the app role can still attempt writes that triggers refuse. |
| Off-system copy of audit records | none | behind | AU-9(2). Everything sits in one database. |
| Metrics | Prometheus text, read-only transaction, loopback default; `TestCheckListen` (`internal/metrics/metrics_test.go:247`), `TestHandler` (`:269`), `TestHostileLabels` (`:158`), `TestCollectLockTimeout` (`:223`) | parity | Safe default. `--allow-remote` exposes unauthenticated metrics with no TLS or auth option; no retention guidance. |
| Traces and OTLP export | none (no OpenTelemetry dependency in `go.mod`; no `traceparent` found in `cmd` or `internal`) | behind | Claude Code exports OTLP and propagates `TRACEPARENT`; control-plane decisions cannot be joined to worker activity by trace id. |
| Structured application logs | No logging library found by search | behind | Not verified further; logs are not a stated audit source. |
| Log export / SIEM sink | none; the TUI and metrics are the read paths | behind | No syslog, JSONL or OTLP logs. |
| Content-capture policy | Redaction at tool-result creation (`internal/agent/loop.go:491`, `TestToolOutputRedactedAtCreation`, `loop_test.go:140`); request captures exclude the key (`TestCaptureNeverSeesKey`, `client_test.go:397`) | parity | Claude Code defaults to redacted content with opt-in; Fabric stores redacted tool output but has no stated per-category policy or opt-in switch. |
| Per-turn cost and request ids | Persisted per turn (`loop.go:373-380`) | parity (peer not documented) | Useful for audit; no export format. |

## Upgrades, ranked
| rank | upgrade | value | effort | gate or risk addressed | ticket: TBD |
|---|---|---|---|---|---|
| 1 | Hash-chain audit rows (previous hash, row hash) and publish a periodic signed checkpoint to a separate store | high | med | owner or superuser can rewrite history undetected | TBD |
| 2 | Dedicated audit-writer role; revoke DML on audit tables from `app`; event trigger or alert on `ALTER TABLE ... DISABLE TRIGGER` | high | low-med | trigger-bypass by owner | TBD |
| 3 | Propagate W3C `traceparent` to workers through the launch profile | med | low | cannot join control plane to worker activity | TBD |
| 4 | OTLP exporter for events and logs, traces with GenAI span names, content off by default | med-high | med | no external observability | TBD |
| 5 | JSONL or syslog export of `effect_resolutions` and `events`, redacted | med | low-med | no off-system copy | TBD |
| 6 | pgAudit guidance for DDL and role changes on audit tables | med | low | schema-level tampering visibility | TBD |
| 7 | Authenticated, TLS metrics path and a retention note; document `--allow-remote` risk | low-med | low | unauthenticated exposure | TBD |

## Not verified
- That the table owner can disable the append-only triggers is stated from Postgres behavior; I did not run it against the schema.
- Whether `app` can `ALTER` or `DISABLE TRIGGER` (it is not the owner by design in `roles.go`); the owner-versus-app split in a deployed database was not inspected.
- Structured logging: only a search for common Go logging libraries was done.
- Rekor and AU-9 claims rely on the cited pages; the NIST text is from a third-party mirror.
