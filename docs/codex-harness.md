# Controlled Codex harness preflight

`fab harness run --adapter codex --codex-preflight --codex-dir /absolute/new-empty-private-directory --events /absolute/new-events.jsonl --wait 30s` checks a dedicated new Codex thread without sending a model turn. The workspace must be empty, owned by the caller, mode0700, and have no symlinked ancestry. The reviewed executable identity is pinned to the existing signed-in Darwin Codex client. No API key, token environment, new login or credential copy is used.

This command currently refuses model execution. The remaining runtime gate requires exact-version capability/backend/hook closure, complete effective configuration and instruction-source verification, and real named-profile filesystem/network denial probes. A profile name or a successful provider reply alone is insufficient. Unsupported or missing readback fails closed. Required user AGENTS and agent-harness hooks remain enabled; enabled inherited MCP/plugins or extra instruction/environment sources cause refusal.

The adapter supports named-profile thread binding and omits legacy per-turn sandbox overrides. The fixed file-edit settings disable shell_tool as well as unified_exec: disabling unified_exec alone can leave one-shot command execution available in this Codex version. It also disables alternate execution, browser, app/plugin, subagent, discovery and memory inputs. Full runtime proof is separate from configuration parsing.

The process leader is retained until group termination and live-member observation finish, and Final is emitted after that cleanup. This handles children remaining in the owned process group; it does not claim Darwin containment of arbitrary double-fork/setsid processes. Arbitrary model process creation must remain excluded before a real file-edit-only job is admitted. The tiny demo is provider-managed execution, not Fabric Linux containment or daemon Codex mission dispatch.

The optional library LifecycleSink projects only this new session's fixed started/completed/failed/stopped states to mc.event/1, source codex_demo. Canonical harness events remain separate. Its caller must create a fresh0600 O_EXCL file and treat every write failure as failure. No prompt, provider text, tool arguments/results, paths or free-form reasons enter the public projection. A partially written run must not be ingested as verified success. Normal CLI ingest/export and deployed MC acceptance remain separate integration steps owned by the release coordinator.

No existing chats, historical WALs or bridge automation are scanned or posted. The disabled global Codex bridge stays disabled.

Focused provider-free checks: `go test -p 1 -count=1 ./internal/harness/codex`. Run builds and Go tests on the fleet under the established resource cap. The old `FABRIC_CODEX_E2E=1` test intentionally refuses its former unrestricted startup; it is not a live-provider PASS.
