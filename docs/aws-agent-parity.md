# Agent-stack parity matrix (AWS, Microsoft, Google) and provider interfaces

Requirement (goodqa#15361): anything the AWS agent stack can do, Fabric can do out of the box, model-agnostic, with no dependency on any one cloud. This page maps the capabilities of Amazon Bedrock AgentCore, Bedrock Guardrails, Bedrock Agents (classic), Knowledge Bases, Flows and Prompt Management, Bedrock evaluations and cost controls onto Fabric's 12 dimensions and the slices F0 to F16 in [status-and-roadmap.md](status-and-roadmap.md) and [slices.md](slices.md). Every GAP and PARTIAL has an acceptance criterion with a `Verify by:` below. A later section maps Microsoft (Agent Framework, Azure AI Foundry Agent Service) and Google (Agent Runtime, ADK, A2A) the same way and defines the provider interfaces that let a tenant swap or mix providers per component (slice F17).

Two constraints from the owner apply to everything here: Fabric stays model-agnostic, and no capability may depend on Google Vertex AI.

## Sources

Read on 2026-11-09. The pages were read through a summarising fetcher, so feature lists come from the page text and numeric limits are quoted as the pages state them. Confidence is medium-high for AgentCore (13 service pages read) and medium for the classic Bedrock items, where the sub-feature lists (for example the guardrail policy types beyond the page's own list) also draw on general product knowledge and should be re-checked against the console before a slice is built.

| Area | URL | Notes |
|---|---|---|
| AgentCore overview | https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/what-is-bedrock-agentcore.html | Lists 13 services: Harness, Runtime, Memory, Gateway, Identity, Code Interpreter, Browser, Observability, Payments, Evaluations, Optimization, Policy, Registry |
| Runtime | .../devguide/agents-tools-runtime.html | microVM per session; 8 hours (microVM) or 14 days (Instances); 100 MB payloads; HTTP and WebSocket; MCP, A2A, AG-UI; versions and endpoints |
| Memory | .../devguide/memory.html, memory-types.html | short-term events, long-term extraction and consolidation, semantic retrieval, event metadata |
| Gateway | .../devguide/gateway.html, gateway-features.html | OpenAPI, Smithy, Lambda to MCP; semantic tool search; sessions, streaming, elicitation, sampling, tasks; model routing |
| Identity | .../devguide/identity.html | workload identities, inbound JWT, outbound credential providers, consent portal |
| Code Interpreter | .../devguide/code-interpreter-tool.html | Python, JavaScript, TypeScript; 15 min default up to 8 hours; 100 MB inline, 5 GB via S3 |
| Browser | .../devguide/browser-tool.html | isolated sessions, live view, recording, 15 min default up to 8 hours |
| Observability | .../devguide/observability.html | OpenTelemetry traces, metrics, audit logging |
| Policy | .../devguide/policy.html | Cedar policies, natural-language authoring, temporal policies, guardrail signals |
| Evaluations, Optimization | .../devguide/evaluations.html, optimization.html | built-in, custom, online, on-demand, batch, dataset, simulation; recommendations, configuration bundles, A/B tests |
| Registry | .../devguide/registry.html | catalog, approval workflow, hybrid search, MCP endpoint |
| Payments | .../devguide/payments.html | x402 and MPP, wallet providers, payment limits |
| Harness | .../devguide/harness.html | managed agent loop, any model, skills, versions and endpoints, export to code |
| Guardrails | https://docs.aws.amazon.com/bedrock/latest/userguide/guardrails-how.html | input and output evaluation; content, denied topics, sensitive information, word, image filters |
| Agents classic | https://docs.aws.amazon.com/bedrock/latest/userguide/agents.html | in maintenance mode, no new sign-ups; action groups, knowledge bases, versions and aliases |
| Knowledge Bases | https://docs.aws.amazon.com/bedrock/latest/userguide/kb-how-it-works.html | ingestion, chunking, embeddings, vector index, retrieval |
| Flows | https://docs.aws.amazon.com/bedrock/latest/userguide/flows.html | nodes, versions, aliases, guardrails, async executions |
| Evaluations | https://docs.aws.amazon.com/bedrock/latest/userguide/evaluation.html | model and RAG evaluation, LLM-as-a-judge, human review |

### Microsoft and Google sources

Read on 2026-11-08 from the current official documentation. "Read" means the page body was fetched; "search" means only a search-result excerpt was seen, which lowers confidence. Naming has drifted: Google renamed Vertex AI and Agent Engine to Gemini Enterprise Agent Platform (Agent Runtime) in April 2026 and moved the ADK docs to adk.dev; Microsoft renamed the Foundry RBAC roles. The old names are used only where a page still does. The AWS sources above were read on 2026-11-09. Managed Agents API (preview), gateway token budgets (announced), the Entra Agent Registry, the Foundry role rename and Bing grounding rest on search excerpts (medium or low confidence); computer use in Foundry is low confidence.

| Area | URL | Confidence |
|---|---|---|
| Agent Framework overview and workflows | https://learn.microsoft.com/en-us/agent-framework/overview/agent-framework-overview, .../concepts/workflows/ | high (read) |
| Foundry Agent Service: overview, hosted agents, toolbox | https://learn.microsoft.com/en-us/azure/foundry/agents/overview, .../agents/concepts/hosted-agents, .../agents/concepts/toolbox-overview | high (read) |
| Foundry identity (Entra Agent ID) | https://learn.microsoft.com/en-us/azure/foundry/agents/concepts/agent-identity | high (read) |
| Foundry IQ (knowledge) and own-resource setup | https://learn.microsoft.com/en-us/azure/foundry/agents/concepts/what-is-foundry-iq, .../agents/how-to/use-your-own-resources | high (read) |
| Foundry browser automation | https://learn.microsoft.com/en-us/azure/foundry/agents/how-to/tools/browser-automation | high (read) |
| Foundry guardrails, AI red teaming agent, tracing | https://learn.microsoft.com/en-us/azure/foundry/guardrails/guardrails-overview, .../concepts/ai-red-teaming-agent, .../observability/concepts/trace-agent-concept | high (read) |
| Foundry memory, model router, control plane, evaluators | .../agents/concepts/what-is-memory, .../openai/concepts/model-router, .../control-plane/overview, .../concepts/built-in-evaluators | medium (search) |
| Foundry runtime components, connected agents, computer use | .../agents/concepts/runtime-components | low (unread or unverified) |
| Gemini Enterprise Agent Platform: overview, runtime, agents | https://docs.cloud.google.com/gemini-enterprise-agent-platform/overview, .../build/runtime, .../agents | high (read) |
| Agent Gateway, governance policies, Agent Registry | https://docs.cloud.google.com/gemini-enterprise-agent-platform/govern/gateways/agent-gateway-overview, .../govern/policies/overview, https://docs.cloud.google.com/agent-registry/overview | high (read) |
| Code execution sandbox, RAG Engine, Agent Studio | .../scale/sandbox/code-execution-overview, .../build/rag-engine/rag-overview, .../agent-studio | high (read) |
| Memory Bank, agent identity, evaluation (agents, simulation, online), computer-use sandbox, Model Armor, Agent Garden | .../scale/memory-bank, .../govern/agent-identity-overview, .../optimize/evaluation/evaluate-agents, .../scale/sandbox/computer-use, https://docs.cloud.google.com/model-armor/model-armor-vertex-integration, .../build/agent-garden | medium (search) |
| Agent Engine overview (earlier name of the runtime) | https://docs.cloud.google.com/vertex-ai/generative-ai/docs/agent-engine/overview | high (read) |
| ADK | https://adk.dev/ | high (read) |
| A2A protocol v1.0 | https://a2a-protocol.org/latest/specification/ | high (first 100k of 256k characters read) |
| AP2 v0.2 (agent payments) | https://ap2-protocol.org/ap2/specification/ | high (read) |
| Rename of Vertex AI and Agent Engine to Gemini Enterprise Agent Platform and Agent Runtime | https://docs.cloud.google.com/gemini-enterprise-agent-platform/release-notes | medium (search) |
| Gemini Enterprise and Agentspace, Example Store, Foundry connected agents | https://cloud.google.com/blog/products/ai-machine-learning/introducing-gemini-enterprise-agent-platform | low (blog or name only; details unread) |

Not covered: Google pricing, Agentspace details, Example Store details, Foundry connected agents and computer use details. Nothing here calls a Microsoft or Google service: it is a documentation read only, and no Vertex or aiplatform access exists in this repository's deployments, CI or tests.

## Baseline: what Fabric has today

Evidence below is `main` at `e409553` plus the open PRs named in the rows (PR 23 codex adapter, PR 25 kimi adapter, PR 26 worker-account controls).

- Durable core: pure reducer, append-only events, outbox, fences and database-time leases (`internal/reducer`, `internal/store`).
- Effect broker with readback and quarantine (`internal/effects`).
- Host sandboxes with negative controls: Landlock and cgroups on Linux, Seatbelt on macOS (`internal/host`).
- Native loop with hold-before-send spend caps and an optional durable tool-call review gate with `shadow`, `evaluate` and `enforce` modes (`internal/agent`, `internal/native`, `internal/reviewgate`).
- Harness adapter seam with native and claude-code adapters merged, codex and kimi in review (`internal/harness`, PRs 20, 22, 23, 25), a JSONL event trace checked by a conformance function, and `fab harness run` deployed (PR 24).
- Model ladder (`internal/ladder`), metrics in Prometheus text, a read-only TUI, RFC 8785 canonical bytes (PR 21).

It is single-host, single-operator and shadow-only. Almost nothing below is HAVE.

## Matrix

Status: HAVE (works today, with the evidence named), PARTIAL (a real piece exists, the AWS capability is not met), GAP (nothing). Slice is where the work lands; `AC` ids are defined in the next section. Slices F14 to F16 are new (below).

### AgentCore Harness and Runtime

| ID | AWS capability | Status | Fabric evidence | Slice and AC |
|---|---|---|---|---|
| RT-1 | Managed agent loop defined by one declarative call (model, prompt, tools) | PARTIAL | Native grant file runs a loop (`internal/native` Grant, `fab mission run`, `fab harness run`); no single declarative agent definition across adapters | F16 AC-AD1 |
| RT-2 | Per-session isolation in its own microVM (CPU, memory, filesystem) | PARTIAL | Per-run sandbox with Landlock, cgroup and negative controls (`internal/host`, 9 controls; same-UID Linux fails 2); no microVM | F5 AC-RT1 |
| RT-3 | Long-running sessions (8 hours; 14 days on Instances), resumable | PARTIAL | Leases, fences, `Resume` of the native loop, interactive claude-code session; no multi-day persistent session across node restart | F5 AC-RT2 |
| RT-4 | Persistent filesystem across stop and resume | GAP | Per-run worktrees only | F5 AC-RT3 |
| RT-5 | Large and multimodal payloads (100 MB) | GAP | Event payloads are text; 4 MiB canonical cap | F8 AC-CP2, F6 AC-RT4 |
| RT-6 | Response streaming and bidirectional WebSocket | PARTIAL | `Adapter.Events` and `Session.Input` in process; no network streaming API | F11 AC-TR3 |
| RT-7 | Protocols: MCP, A2A, AG-UI | GAP | none | F8 AC-GW2, F9 AC-MB4 |
| RT-8 | Framework agnostic (LangGraph, CrewAI, Strands, custom) | GAP | adapters wrap harnesses, not frameworks | F6 AC-RT5 |
| RT-9 | Any model, switch provider mid-session | PARTIAL | Native loop is Anthropic only; claude-code, codex, kimi adapters; ladder picks a rung | F10 AC-GA1, F6 AC-RT6 |
| RT-10 | Versions and named endpoints, instant rollback | GAP | binary rollback only | F16 AC-AD2 |
| RT-11 | Skills attached to an agent | GAP | harness-native skills only (claude-code) | F16 AC-AD3 |
| RT-12 | Bring your own container image; custom environment | GAP | none | F5 AC-RT7 |
| RT-13 | Mount shared storage across sessions | GAP | none | F8 AC-CP3 |
| RT-14 | Interactive shell into a session | PARTIAL | allow-listed `shell` tool; interactive adapters | F8 AC-CI3 |
| RT-15 | Scale to zero, consumption billing | PARTIAL | admission, caps, leases; no placement or idle shutdown | F5 AC-RT8, F10 AC-CO2 |
| RT-16 | Lifecycle hooks | PARTIAL | policy oracle and hooks dir | F7 AC-PL1 |
| RT-17 | Export an agent to code | GAP | none | F16 AC-AD4 |
| RT-18 | Multi-agent on one runtime | PARTIAL | 1 writer plus 1 reviewer per mission | F12 AC-MA1 |

### AgentCore Memory

| ID | AWS capability | Status | Fabric evidence | Slice and AC |
|---|---|---|---|---|
| ME-1 | Short-term memory: events per session, list sessions and events | PARTIAL | native loop stores messages and tool calls durably per attempt; harness traces; no session API | F11 AC-TR1, F14 AC-ME1 |
| ME-2 | Event metadata and filtered listing | GAP | none | F14 AC-ME1 |
| ME-3 | Long-term memory: extraction and consolidation (semantic, summary, preference, episodic) | GAP | none | F14 AC-ME2 |
| ME-4 | Semantic retrieval of memory records | GAP | none | F14 AC-ME3 |
| ME-5 | Memory shared across agents, namespaces, tenant isolation | GAP | none | F14 AC-ME4, F3 |
| ME-6 | Custom and self-managed strategies | GAP | none | F14 AC-ME5 |
| ME-7 | Retention, deletion, encryption, no sensitive data in metadata | GAP | none | F14 AC-ME6 |

### AgentCore Gateway and Registry

| ID | AWS capability | Status | Fabric evidence | Slice and AC |
|---|---|---|---|---|
| GW-1 | Convert OpenAPI, Smithy and Lambda-style functions into tools | GAP | tools are Go code (`internal/tools`: read, grep, shell) | F8 AC-GW1 |
| GW-2 | Connect existing MCP servers as tools | GAP | none | F8 AC-GW2 |
| GW-3 | Semantic tool search over thousands of tools | GAP | none | F8 AC-GW3 |
| GW-4 | One-click integrations (Slack, Jira, GitHub, Salesforce-like) | GAP | GoodQA ticket comment effect only (`internal/effects`) | F8 AC-GW4 |
| GW-5 | Inbound auth (agent identity) and outbound auth (credential exchange per tool) | GAP | provider key kept out of the sandbox, redacted (`internal/native/key.go`) | F4 AC-ID3, F8 AC-GW5 |
| GW-6 | Model routing and inference gateway | PARTIAL | egress mediator (`internal/egress`: host:port allow-list, SNI match, default only the model API) wired into the Linux sandbox, ladder (`internal/ladder`); no model gateway | F10 AC-GA1 |
| GW-7 | MCP sessions, streaming, elicitation, sampling, long-running tasks | GAP | none | F8 AC-GW6 |
| GW-8 | Agent-to-agent passthrough | GAP | none | F9 AC-MB4 |
| GW-9 | Interceptors and response transformation | PARTIAL | policy oracle hooks | F7 AC-PL1 |
| GW-10 | Gateway audit logging | PARTIAL | append-only events, commands, effect resolutions; no hash chain | F11 AC-TR2 |
| GW-11 | Tool and agent registry with approval workflow, hybrid search, MCP endpoint | GAP | none | F8 AC-GW7 |
| GW-12 | Registry notifications and cross-account sharing | GAP | none | F9 AC-MB5, F3 |

### AgentCore Identity (in full) and user-delegated access

| ID | AWS capability | Status | Fabric evidence | Slice and AC |
|---|---|---|---|---|
| ID-1 | Workload identities for agents | GAP | actors are self-asserted strings (`internal/domain/ids.go`) | F4 AC-ID1 |
| ID-2 | Inbound auth: OIDC and JWT from any IdP, Cognito-style user pools | GAP | none | F4 AC-ID2 |
| ID-3 | Outbound credential providers: OAuth 2LO and 3LO, API keys | GAP | provider key via `key_command` only | F4 AC-ID3 |
| ID-4 | Token vault: encrypted storage, refresh, per-agent access limits | GAP | none; secrets come from an external manager at call time | F4 AC-ID4 |
| ID-5 | User-delegated access (on behalf of a user) with consent | PARTIAL | durable human approval of tool calls (`internal/reviewgate`); no delegation or consent flow | F4 AC-ID5 |
| ID-6 | Consent portal and step-up | GAP | none | F4 AC-ID5, F13 |
| ID-7 | Private IdP connectivity | GAP | none | F4 AC-ID2 |
| ID-8 | Signed, expiring, request-bound approvals and separation of duty | GAP | approver is free text (`internal/effects/broker.go`) | F4 AC-ID6 |

### Guardrails (Bedrock Guardrails), on the F7 policy engine and F10 gateway

| ID | AWS capability | Status | Fabric evidence | Slice and AC |
|---|---|---|---|---|
| GR-1 | Evaluate input and output of every model call | GAP | none; native loop redacts secrets in tool results only | F10 AC-GU1 |
| GR-2 | Content filters (hate, insults, sexual, violence, misconduct) with strengths | GAP | none | F7 AC-GU2 |
| GR-3 | Prompt-attack and jailbreak detection | GAP | none | F7 AC-GU3 |
| GR-4 | Denied topics | GAP | none | F7 AC-GU2 |
| GR-5 | Word and phrase filters | GAP | none | F7 AC-GU2 |
| GR-6 | Sensitive information: PII detection, block or mask, custom regex | PARTIAL | secret-shaped redaction in tool results and events (`internal/harness` redactors, key ring) | F7 AC-GU4 |
| GR-7 | Contextual grounding and relevance checks | GAP | none | F7 AC-GU5, F14 |
| GR-8 | Automated reasoning checks | GAP | none; reducer invariants are deterministic code, not a user rule checker | F7 AC-GU6 |
| GR-9 | Guardrails on tool calls and tool results | PARTIAL | policy oracle judges shell only; review gate covers native tool calls | F7 AC-GU7 |
| GR-10 | Detect-only mode, blocked message, streaming evaluation | PARTIAL | review gate `shadow` and `evaluate` modes (native tool calls only) | F7 AC-GU8 |
| GR-11 | Model-independent apply API (ApplyGuardrail) | GAP | none | F10 AC-GU1 |
| GR-12 | Image content filters | GAP | none | F7 AC-GU2 (deferred behind F8 artifacts) |

### Policy, Evaluations, Optimization (AgentCore)

| ID | AWS capability | Status | Fabric evidence | Slice and AC |
|---|---|---|---|---|
| PO-1 | Deterministic policy evaluated outside the agent at every tool call | PARTIAL | policy oracle (shell only), review gate, sandbox; constants for caps | F7 AC-PL1 |
| PO-2 | Policy language with validation (Cedar) and natural-language authoring | GAP | none | F7 AC-PL2 |
| PO-3 | Fine-grained by principal and tool input | GAP | none | F7 AC-PL3 |
| PO-4 | Temporal, session-aware rules (counts, budgets, approval before action) | PARTIAL | spend caps, approval for effects | F7 AC-PL4 |
| PO-5 | Recorded decisions with audit | PARTIAL | review gate audit rows | F7 AC-PL5 |
| EV-1 | Built-in evaluators on sessions, traces, spans | GAP | none | F15 AC-EV1 |
| EV-2 | Custom evaluators (LLM judge, code) | GAP | none | F15 AC-EV2 |
| EV-3 | Online, on-demand, batch, dataset evaluation and simulation | GAP | none | F15 AC-EV3 |
| EV-4 | Verification before success | PARTIAL | live-test receipt gate for `done`; verification receipts planned (goodqa#15325) | F15 AC-EV4 |
| OP-1 | Recommendations: prompt and tool-description improvement from traces | GAP | none | F15 AC-OP1 |
| OP-2 | Versioned configuration bundles | GAP | none | F16 AC-AD2 |
| OP-3 | A/B tests with traffic split and significance | GAP | none | F15 AC-OP2 |

### Code Interpreter, Browser, Payments (AgentCore)

| ID | AWS capability | Status | Fabric evidence | Slice and AC |
|---|---|---|---|---|
| CI-1 | Sandboxed code execution (Python, JavaScript, TypeScript) | PARTIAL | allow-listed `shell` in a host sandbox | F8 AC-CI1 |
| CI-2 | File upload and download, large files | GAP | none | F8 AC-CP2 |
| CI-3 | Session timeouts and network modes | PARTIAL | wall caps, host:port egress allow-list with SNI match in the Linux sandbox | F8 AC-CI2 |
| CI-4 | Custom interpreter images | GAP | none | F5 AC-RT7 |
| BR-1 | Isolated managed browser sessions (Playwright, CDP) | GAP | none | F8 AC-BR1 |
| BR-2 | Live view and human takeover | GAP | none | F8 AC-BR2 |
| BR-3 | Session recording and replay | GAP | none | F8 AC-BR3 |
| BR-4 | Timeouts, concurrency, profiles, proxies | GAP | none | F8 AC-BR1 |
| PY-1 | Pay-per-use payments (x402, MPP) with wallets | GAP | none; money domain, needs an owner decision | F8 AC-PY1 (gated) |
| PY-2 | Payment limits per user and agent | PARTIAL | hold-before-send model spend caps (`internal/agent/ledger.go`) | F10 AC-CO1, F8 AC-PY1 |

### Observability (AgentCore)

| ID | AWS capability | Status | Fabric evidence | Slice and AC |
|---|---|---|---|---|
| OB-1 | OpenTelemetry traces and spans | GAP | none; no trace ids | F1 AC-5, F11 AC-TR3 |
| OB-2 | Metrics: sessions, latency, tokens, errors | PARTIAL | `fab metrics`, `fab serve-metrics` (`internal/metrics`) | F11 AC-TR4 |
| OB-3 | Dashboards and session views | PARTIAL | `fab tui`, `fab status` | F11 AC-TR4 |
| OB-4 | Audit log of API activity, tamper evidence | PARTIAL | append-only tables; no hash chain, no export | F11 AC-TR2 |
| OB-5 | Token and cost accounting | PARTIAL | `usage_receipts`, spend ledger for the native loop; subscription runs unmetered | F10 AC-CO1 |
| OB-6 | Cross-account monitoring | GAP | none | F3, F11 AC-TR5 |

### Bedrock Agents (classic)

| ID | AWS capability | Status | Fabric evidence | Slice and AC |
|---|---|---|---|---|
| AG-1 | Action groups (API schema or function definitions, executors) | GAP | tools are code | F8 AC-GW1, F16 AC-AD1 |
| AG-2 | Return of control (hand an action back to the caller) | PARTIAL | pending review pause returns control to a human (`OutcomeApprovalRequired`); no caller-side action return | F16 AC-AG1 |
| AG-3 | Multi-agent collaboration: supervisor and routing | GAP | none | F12 AC-MA2 |
| AG-4 | Session attributes and prompt-session attributes | GAP | none | F16 AC-AG2 |
| AG-5 | Versions and aliases | GAP | none | F16 AC-AD2 |
| AG-6 | Orchestration prompt overrides | PARTIAL | native grant `system`, `extra` fields | F16 AC-AD1 |
| AG-7 | Agent trace of reasoning steps | PARTIAL | harness event trace; reasoning not emitted by design | F11 AC-TR3 |
| AG-8 | Knowledge base association | GAP | none | F14 AC-KB1 |
| AG-9 | Inline agents (define at invoke time) | PARTIAL | `fab harness run` takes a grant at run time | F16 AC-AD1 |

Note: AWS has put Agents classic into maintenance mode; the AgentCore Harness is its successor. Parity here means the capabilities, not API compatibility.

### Knowledge Bases (RAG)

| ID | AWS capability | Status | Fabric evidence | Slice and AC |
|---|---|---|---|---|
| KB-1 | Data sources and ingestion sync | GAP | none | F14 AC-KB1 |
| KB-2 | Parsing and chunking strategies (fixed, hierarchical, semantic, custom) | GAP | none | F14 AC-KB2 |
| KB-3 | Embeddings and vector store (any model, binary or float) | GAP | none | F14 AC-KB3 |
| KB-4 | Retrieve and retrieve-and-generate with citations | GAP | none | F14 AC-KB4 |
| KB-5 | Metadata filters, hybrid search | GAP | none | F14 AC-KB4 |
| KB-6 | Reranking and query reformulation | GAP | none | F14 AC-KB5 |
| KB-7 | Structured-data retrieval (text to SQL) | GAP | none | F14 AC-KB6 |
| KB-8 | Per-tenant isolation and deletion of sources | GAP | none | F14 AC-KB7, F3 |

### Flows and Prompt Management

| ID | AWS capability | Status | Fabric evidence | Slice and AC |
|---|---|---|---|---|
| FL-1 | Workflow of nodes (prompt, agent, knowledge base, condition, iterator, function) | GAP | the reducer is a work-run state machine, not a user workflow | F16 AC-FL1 |
| FL-2 | Immutable versions and aliases with instant rollback | GAP | none | F16 AC-AD2 |
| FL-3 | Asynchronous flow executions with status | PARTIAL | missions run unattended and durably | F16 AC-FL2 |
| FL-4 | Guardrails inside a flow | GAP | none | F7 AC-GU7, F16 AC-FL1 |
| FL-5 | Prompt management: variants, versions, templates with variables | GAP | grant strings only | F16 AC-PM1 |
| FL-6 | Prompt optimisation | GAP | none | F15 AC-OP1 |
| FL-7 | Multi-turn conversation with a flow | GAP | none | F16 AC-FL2 |

### Bedrock evaluations (model and RAG) and cost

| ID | AWS capability | Status | Fabric evidence | Slice and AC |
|---|---|---|---|---|
| BE-1 | Automatic model evaluation with datasets and metrics | GAP | none | F15 AC-EV3 |
| BE-2 | LLM-as-a-judge (correctness, faithfulness, harmfulness, refusal) | GAP | none | F15 AC-EV2 |
| BE-3 | RAG evaluation (retrieval only, retrieve and generate, ground truth) | GAP | none | F15 AC-EV5 |
| BE-4 | Human evaluation workflow | GAP | none | F15 AC-EV6 |
| BE-5 | Bring-your-own inference | PARTIAL | adapters give traces from any harness | F15 AC-EV3 |
| CO-1 | Per-job, per-run, per-month spend controls | PARTIAL | USD 20 month, 5 job, 1 live test, hold-before-send in the native loop only (`internal/native/native.go`, `internal/agent/ledger.go`, test `TestBudgetHeldBeforeSend`); caps are bypassable while goodqa#15123 and #15124 are open, and the `claude -p` path is unmetered | extend F10 AC-CO1 |
| CO-2 | Cost allocation by tenant, project, agent; usage reports | GAP | spend is per attempt, mission, month only | F10 AC-CO1, F3 |
| CO-3 | Quotas and rate limits per tenant | GAP | none | F10 AC-CO3, F3 |
| CO-4 | Prompt caching, batch, tier selection, cost-aware routing | PARTIAL | cache tokens priced in the ledger; ladder routes by rung, not cost | F10 AC-CO2 |
| CO-5 | Budget alerts and automatic stop | PARTIAL | caps stop a run; no alerting | F10 AC-CO4 |

## Summary

Counts are taken from the tables above (see the check at the end of this page).

| Status | Rows |
|---|---|
| HAVE | 0 |
| PARTIAL | 39 |
| GAP | 96 |
| Total | 135 |

Read this as the honest state: Fabric has the durable kernel and a few control points (spend caps, a review gate, sandbox controls, an event trace). It lacks the surrounding platform: identity, guardrails, a tool gateway, memory and knowledge, evaluation, browser and code services, versions and aliases, and OpenTelemetry.

## Slice additions

Existing slices keep their scope. The additions below are acceptance criteria folded into F1 to F13, plus three new slices where nothing fits.

New slices:

- **F14 Memory and knowledge.** Session memory, long-term memory strategies, retrieval, knowledge-base ingestion and RAG. Dimensions: traceable, long-term work tracking, multi-tenant.
- **F15 Evaluation and optimisation.** Evaluators, online and batch evaluation, RAG and agent evaluation, LLM-as-judge, recommendations, A/B tests. Dimensions: traceable, policy.
- **F16 Agent definitions, prompts and flows.** Declarative agent definition, immutable versions and aliases, prompt management, workflows, return of control, session attributes. Dimensions: multi-harness, multi-agent.

Conventions: every AC below carries `Verify by:`; a negative case follows where one decides whether the gate is real. "Fresh install" is defined by AC-OOTB at the end.

### F4 Principals and identity

- AC-ID1: an agent has a workload identity (issuer, subject, tenant) that is not a free-text actor; a command from an unknown identity is refused. Verify by: `go test ./internal/identity -run TestWorkloadIdentity` and a forged `FABRIC_ACTOR` negative. NEG: a revoked identity is refused on its next command even though its token has not expired.
- AC-ID2: inbound auth accepts a signed OIDC or JWT from a configured issuer (JWKS fetch, audience, expiry, clock skew) and from the `local` issuer. Verify by: tests with a local JWKS server: valid, expired, wrong audience, wrong issuer, `alg` none, key rollover. NEG: an unreachable JWKS fails closed.
- AC-ID3: outbound credential providers for OAuth client credentials (2LO), OAuth authorization code (3LO) and API keys, per tool, resolved by the gateway, never placed in the sandbox environment or the trace. Verify by: a fake OAuth server; a canary secret never appears in the trace, the events or the child environment. NEG: a tool for tenant A cannot resolve tenant B's provider.
- AC-ID4: a token vault: credentials encrypted at rest, refreshed before expiry, readable only by the broker role, access recorded. Verify by: a refresh test with a short-lived token; a database dump contains no plaintext; the app role cannot read the table. NEG: a stolen sandbox cannot call the vault.
- AC-ID5: user-delegated access: an action carries `on_behalf_of`; a 3LO grant requires a recorded user consent; consent expires and is revocable; step-up consent for a named tool class. Verify by: a delegation test with consent given, expired and revoked. NEG: an agent cannot use a user's token for a tool outside the consented scope.
- AC-ID6: approvals are signed, expiring and bound to the request hash; the requester cannot approve (separation of duty). Verify by: tests in F4's own spec (forged actor, expired approval, replay, wrong hash).

### F7 Policy engine and guardrails

- AC-PL1: every tool call, model call and effect passes one decision point; the harness hooks call it, and an unreachable decision point denies. Verify by: `go test ./internal/policy -run TestDecisionPoint` and a killed-service negative.
- AC-PL2: policy bundles in a validated language. Decision: Cedar-compatible, or a Fabric JSON bundle with a Cedar importer (needs the owner's choice; default proposed: Fabric JSON bundle first, Cedar import as a later AC). Verify by: schema validation tests, a bundle with an unknown class refused at load, and natural-language authoring producing a candidate bundle that must pass validation and a safety check (overly permissive, never satisfiable) before it is accepted.
- AC-PL3: rules on principal, capability class, tool and input parameters. Verify by: a table test of allow, deny and require-approval over input values.
- AC-PL4: temporal rules over session history (count limits, running totals, approval before action). Verify by: a scripted session test; NEG: a count limit is not reset by starting a new attempt of the same mission.
- AC-PL5: each decision is recorded with the bundle digest, principal, class and subject. Verify by: event assertions; NEG: a decision cannot be edited after the fact (append-only).
- AC-GU2: a guardrail provider interface and a built-in local provider for content filters (categories with strengths), denied topics and word and phrase filters, applied to model input, model output, tool input and tool output, configurable per tenant, agent and tool. Verify by: fixture corpora per category: a blocked input never reaches the model (zero upstream requests); a blocked output is replaced by the configured message. NEG: the provider failing closed blocks, and failing open is only possible by an explicit bundle setting that is recorded.
- AC-GU3: prompt-attack and jailbreak detection as a provider signal usable in rules, including attacks arriving through tool output (indirect injection). Verify by: a labelled fixture set with a pre-registered threshold and sample size reported with a Wilson lower bound; no claim beyond it.
- AC-GU4: sensitive-information handling: PII entity detection (names, emails, phone, addresses, payment cards with Luhn check, government ids) and custom regex, with block or mask, on input and output and in trace and memory writes. Verify by: fixtures for each entity; masking is irreversible in the stored trace; a canary PII string never reaches the event store. NEG: PII in a tool argument is masked before an effect is dispatched when the rule says mask.
- AC-GU5: contextual grounding and relevance checks of an answer against supplied source text, with a score and threshold. Verify by: grounded and ungrounded fixtures; the check is a provider, so any model can back it.
- AC-GU6: rule-checking ("automated reasoning") of an agent answer against a user-supplied formal rule set, with a counterexample on failure. Verify by: a small rule set (arithmetic and enumerated constraints) with passing and failing answers. Scope note: AWS's product is a formal-methods service; Fabric's AC is the verifiable subset, labelled as such.
- AC-GU7: guardrails apply to tool calls and tool results (including inside a workflow node). Verify by: a tool whose result carries an injection string is blocked before it reaches the model.
- AC-GU8: modes detect-only, enforce and a blocked-message setting, and streaming evaluation (asynchronous and synchronous). Verify by: detect-only records and never blocks; enforce blocks; a streamed answer is cut at the violating chunk.

### F10 Gateway, cost and routing

- AC-GA1: a per-node model gateway applies the guardrail provider to every model call (input and output) whatever the model or harness, records model, request digest, usage and latency, and routes across providers. Verify by: a fake upstream sees zero requests for a blocked input; routing follows the ladder and a configured fallback; a Vertex provider cannot be registered (a negative test for the owner rule).
- AC-GU1: an apply API: `fab guard apply --guardrail G --text T` returns the decision for any text, independent of any model. Verify by: CLI test.
- AC-CO1: cost and usage controls per tenant, project, agent and run: hold-before-send caps (existing), allocation tags on every usage record, and a usage report. Verify by: two-tenant usage totals; caps still hold under concurrent calls (existing property test extended).
- AC-CO2: cost-aware routing and caching: the router can choose a cheaper rung for a task class within quality limits set by an evaluator, and cache-token pricing is applied. Verify by: a routing table test; no downgrade when the evaluator floor is not met.
- AC-CO3: per-tenant quotas and rate limits (tokens per minute, requests, concurrent runs). Verify by: a burst test returns 429 and sends nothing upstream.
- AC-CO4: budget alerts and automatic stop at thresholds. Verify by: an alert event at 80 percent and a stop at 100 percent, recorded as events.

### F8 Capability broker, tool gateway, registry, browser and code

- AC-GW1: OpenAPI-described HTTP APIs and local functions become tools through the broker, with typed schemas and per-tool grants. Verify by: a fake API; a call outside its grant is refused; a mutating call goes through the effect state machine with readback.
- AC-GW2: external MCP servers connect as tool providers (stdio and HTTP), with the MCP session, streaming and long-running task features. Verify by: a conformance run against a reference MCP server fixture; a server that hangs is cut by a lease.
- AC-GW3: semantic tool search over the registered tools, returning the top tools for a task. Verify by: a labelled query set with recall at 5 reported with a Wilson bound; searching 1,000 synthetic tools stays under a stated latency.
- AC-GW4: integration templates (starting set: GoodQA, GitHub, Slack, Jira) installable by configuration. Verify by: each template passes the same tool contract test against a recorded fixture.
- AC-GW5: outbound auth per tool through F4 credential providers. Verify by: the F4 AC-ID3 tests applied through the broker.
- AC-GW6: MCP elicitation and sampling are routed through the policy engine and the human approval path. Verify by: an elicitation request creates an inbox item and a pending state, not a tool failure.
- AC-GW7: a registry of tools, agents and skills with an approval workflow (draft, pending, approved, deprecated), hybrid semantic and keyword search, and an MCP endpoint. Verify by: only approved records are discoverable; a deprecated record disappears from search; search honours tenant isolation.
- AC-CP2: artifacts with digests and large-file support (streamed, resumable, bounded); a mutated artifact fails its digest. Verify by: a 1 GiB streamed round trip within a stated memory bound; tamper test.
- AC-CP3: shared storage mounts across sessions through an artifact store with history. Verify by: two sessions see each other's committed writes; a deleted file is recoverable from history.
- AC-CI1: a code-execution capability for Python, JavaScript and TypeScript in the host sandbox with per-session isolation. Verify by: language fixtures; a session cannot read another session's files.
- AC-CI2: session timeouts (default and extended) and network modes (none, allow-list). Verify by: a timeout kills the whole process group; a network call outside the allow-list fails.
- AC-CI3: an interactive shell session capability with the same isolation. Verify by: a scripted terminal session; Stop ends all descendants.
- AC-BR1: a browser capability with isolated sessions, Playwright and CDP access, timeouts and concurrency limits, an external GoodQA browser provider. Verify by: a disposable page; a session cannot read another session's cookies.
- AC-BR2: live view and human takeover through an external GoodQA remote-session route with a recorded handoff. Verify by: a takeover test records who took control and when; the agent is paused during takeover.
- AC-BR3: session recording and replay stored as artifacts with digests. Verify by: replay reproduces the action log; tamper fails the digest.
- AC-PY1 (gated, owner decision first): payment capability classes with per-session spend limits and wallet providers behind the credential vault. This is the money domain: it needs the owner's explicit product decision and its own spec, with the money checklist (idempotency, double submit, reversal, reconciliation) before any code. Verify by: defined in that spec.

### F5, F6, F9, F11, F12, F13, F1, F3 additions

- AC-RT1 (F5): a sandbox driver interface with host profiles and a container driver; a per-session isolation proof (one session cannot see another's process, file or network namespace). Verify by: the negative control suite run per driver.
- AC-RT2 (F5): a session survives a node restart: after killing the node, another node resumes the session from the event log with a new fence. Verify by: a kill-and-resume test with no double-executed side-effecting tool.
- AC-RT3 (F5): a persistent session filesystem across stop and resume. Verify by: files written before Stop exist after resume; a different tenant cannot mount it.
- AC-RT7 (F5): bring-your-own container image as a sandbox environment, pinned by digest. Verify by: a pinned image runs; an unpinned or mismatching digest is refused.
- AC-RT8 (F5): idle sessions are suspended and resources released; admission counts only active sessions. Verify by: a suspend and resume test with a slot-count assertion.
- AC-RT4 (F6): multimodal input to adapters that support it (text, image, file reference) with size caps. Verify by: an oversize input is refused before sending.
- AC-RT5 (F6): a framework adapter contract: any process speaking the documented stream-json or HTTP agent protocol is an adapter without code in Fabric. Verify by: a sample LangGraph-style process in a fixture passes the conformance suite.
- AC-RT6 (F6): switching the model provider mid-session without losing context for adapters that expose message history. Verify by: a session that plans with one adapter and continues with another keeps its message history.
- AC-MB4 (F9): an A2A bridge: a Fabric agent is addressable over A2A and can call external A2A agents under policy. Verify by: an A2A conformance fixture.
- AC-MB5 (F9): events and notifications (record submitted, approval needed) delivered through the message bus. Verify by: a subscriber receives each exactly once.
- AC-TR1, AC-TR3 (F11): a read API listing sessions and events with filters on event metadata; streaming over server-sent events and WebSocket; OpenTelemetry export mapping trace and span ids. Verify by: an OpenTelemetry collector fixture receives spans whose ids match the stored envelope; filtered listing returns exactly the matching events.
- AC-TR2 (F11): a hash chain over the stored canonical bytes with tamper detection and an off-system export. Verify by: tamper test and an anchored head hash.
- AC-TR4 (F11): metrics for sessions, latency, token usage and error rates per tenant and agent, and a session view. Verify by: counters match the event log for a scripted run.
- AC-TR5 (F11 with F3): cross-tenant administrator views for an organisation, never across organisations. Verify by: two-organisation API test.
- AC-MA1, AC-MA2 (F12): more than two live attempts under a role policy, and a supervisor agent that routes a task to collaborator agents (route to one, or fan out). Verify by: an oracle property test with seeded traces; a supervisor whose collaborator is cancelled settles deterministically.
- AC-5 (F1): every event carries a W3C `trace_id` and `span_id`. Verify by: a store test that rejects an envelope without them, plus a round-trip against fixed vectors.

### F14 Memory and knowledge (new)

- AC-ME1: session memory: create events with metadata, list sessions, list events with metadata filters, get one event. Verify by: API tests; metadata is not encrypted and the doc says never store secrets in it; a secret-shaped value in metadata is refused.
- AC-ME2: long-term memory generation as an asynchronous, restart-safe job with extraction and consolidation and strategies: semantic facts, summaries, user preferences, episodic. The extractor is any model through the F10 gateway. Verify by: a fixture conversation yields the expected records with a pre-registered scoring threshold; the job is idempotent on retry; no duplicate records.
- AC-ME3: semantic retrieval over memory records with namespace filters. Verify by: a labelled retrieval set with recall at 5 and a Wilson bound; a query never returns another namespace's record.
- AC-ME4: memory shared across agents within a namespace, isolated across tenants. Verify by: two agents share; two tenants never see each other's records (row-level security test).
- AC-ME5: custom and self-managed strategies: a user-supplied extraction prompt and model, and direct ingestion of records. Verify by: a custom strategy produces records; direct ingestion bypasses extraction and is audited.
- AC-ME6: retention and deletion: per-namespace retention, a delete that removes records and their embeddings and is verifiable, encryption at rest, a tenant export. Verify by: a deleted record is unreadable and absent from the index; a restore from backup does not resurrect it past the documented window.
- AC-KB1: knowledge-base data sources and ingestion sync: files, web pages, GoodQA, and a custom connector, with change detection. Verify by: re-sync ingests only changes; a failed document is reported and does not stop the batch.
- AC-KB2: parsing and chunking strategies: fixed, hierarchical, semantic and a custom function, with overlap. Verify by: golden chunk outputs per strategy.
- AC-KB3: embeddings through any provider and a vector store behind an interface (Postgres vector extension by default, so a personal install needs no extra service). Verify by: swapping the embedding provider re-embeds and retrieval still passes the quality set.
- AC-KB4: retrieve, and retrieve-and-generate with citations; metadata filters and hybrid (keyword and vector) search. Verify by: every cited span exists in the cited source; a filter excludes non-matching chunks.
- AC-KB5: reranking and query reformulation as optional stages with measured effect. Verify by: a quality set shows the stage's effect; disabling it restores the baseline.
- AC-KB6: structured-data retrieval: natural language to read-only SQL against registered schemas under policy. Verify by: a write statement is refused; a query outside the registered schema is refused.
- AC-KB7: per-tenant isolation and source deletion that removes derived chunks and embeddings. Verify by: the F3 row-level security test on the chunk and vector tables.

### F15 Evaluation and optimisation (new)

- AC-EV1: built-in evaluators over sessions, traces and spans: goal success, tool-call correctness, helpfulness, harmfulness, refusal, faithfulness. Verify by: a labelled fixture set per evaluator with a pre-registered agreement threshold.
- AC-EV2: custom evaluators: an LLM-as-judge prompt with any model through the gateway, and a code evaluator run in the sandbox. Verify by: a judge disagreement rate on a labelled set is reported; a code evaluator cannot reach the network.
- AC-EV3: online evaluation (sampled, continuous), on-demand, batch, dataset, and simulation (scripted user). Verify by: a sampled run scores about the configured fraction of sessions; results are stored as events.
- AC-EV4: verification before success: a work run reaches `SUCCEEDED` only with a verification receipt bound to the produced head; the verifier is not the worker (goodqa#15325). Verify by: that ticket's own spec.
- AC-EV5: RAG evaluation: retrieval-only and retrieve-and-generate with ground truth, reporting context relevance, context coverage, correctness, faithfulness and citation precision. Verify by: a small ground-truth set with known failures is scored correctly.
- AC-EV6: a human evaluation workflow through the inbox: assign, rate, adjudicate. Verify by: ratings are attributed to authenticated principals and cannot be edited after submission.
- AC-OP1: recommendations: propose a changed system prompt or tool description from failing traces and a target evaluator, as a candidate version, never auto-applied. Verify by: a candidate is produced and held for approval; an applied candidate shows an evaluator delta on a held-out set.
- AC-OP2: A/B tests: traffic split between two versions or endpoints with online evaluation and a significance report. Verify by: a simulated experiment with a known effect is detected at the stated power; no split without a recorded approval.

### F16 Agent definitions, prompts and flows (new)

- AC-AD1: one declarative agent definition (model or adapter, prompt, tools, skills, guardrails, memory, knowledge bases) in a file, runnable with `fab agent run` on any adapter; an inline form at invoke time. Verify by: the same definition runs on native and claude-code adapters in tests and produces conforming traces.
- AC-AD2: immutable versions and named aliases with instant rollback and configuration bundles (prompt, model, tool descriptions) that change behaviour without a redeploy. Verify by: an alias moves between versions with no downtime in a test; an in-flight session keeps its version; a version cannot be edited.
- AC-AD3: skills attachable by reference (git path or artifact digest). Verify by: a skill pinned by digest loads; a mismatching digest is refused.
- AC-AD4: export of a definition to a standalone file that runs without a Fabric server (the definition and an adapter command). Verify by: the exported file reproduces a run on a fresh machine.
- AC-AG1: return of control: a tool call can be returned to the caller as a pending action (with typed parameters) and resumed with the caller's result. Verify by: a pending action survives a restart and resumes once.
- AC-AG2: session attributes and prompt-session attributes: key-value state set by the caller, available to tools and prompts, scoped to the session, not stored in clear when marked sensitive. Verify by: attributes are visible to tools in the session and absent from other sessions.
- AC-FL1: workflows of nodes (prompt, agent, knowledge base, condition, iterator, function, guardrail) defined in a file, validated before run, executed as a work stream. Verify by: a branching and looping fixture runs deterministically under the oracle; a cycle without a bound is refused at validation.
- AC-FL2: asynchronous executions with status, cancellation and multi-turn conversation with a workflow. Verify by: start, poll, cancel and resume tests; a cancelled execution leaves no running attempt.
- AC-PM1: prompt management: templates with variables, variants, versions and aliases, with a diff and an evaluator hook. Verify by: a variable missing at render time is an error; a version is immutable.

## Design inputs from internal prototypes

The owner mined earlier internal agent prototypes for designs worth reusing. They are folded into the criteria above as design constraints, not as new tickets and not as copied code. Names of the source repositories, evidence trees and production addresses stay out of this repository.

| Pattern | Folded into | Constraint it adds |
|---|---|---|
| Governed-action laws: one closed path from request to effect, intent recorded before the effect, an explicit unknown outcome settled by readback, single-use grants | F4 AC-ID6, F7 AC-PL1, F8 AC-GW1 | a grant is consumed once; a replay is refused; an effect whose outcome is unknown is never retried blind |
| Per-subject hash-chained record with closed event kinds, an allow-list sanitiser, a golden vector, and appends in the caller's transaction | F1, F11 AC-TR2 | the chain covers digests and references, never content; the sanitiser is an allow-list; a node keeps a durable local outbox so records are not lost while the control plane is down |
| Signed checkpoints with an independent minimum-sequence floor, plus a Merkle anchor | F11 AC-TR2 | a verifier fails when the stored head is below the signed floor (truncation), not only on edited rows |
| Three-layer authorisation: default-deny policy, approval bound to a request fingerprint, signed allow-list | F4 AC-ID6, F7 AC-PL1 | each layer can deny alone; approval is useless for a different fingerprint |
| Signed policy releases with separation of duties and case evidence; one-time-nonce, doubly signed pause | F7 AC-PL5, F4 | a policy bundle activates only with a second principal's signature and passing synthetic cases; a pause token is single use |
| Staged model pipeline: typed stages, a post stage that cannot deny, circuit-breaker rules, budget admission separate from accounting, atomic reserve, settle and reconcile | F10 AC-GA1, AC-CO1 | admission and accounting are separate steps; settle never exceeds the hold; reconcile fixes drift against the provider |
| Capability tokens: scoped signed tokens, instant revocation, argument-digest audit, a frozen tool list | F8 AC-GW1, AC-GW5, F4 AC-ID3 | revocation takes effect on the next call; audit records a digest of arguments, not the arguments |
| Attested node profile: capability schema with a fingerprint, fail-closed tool isolation by OS principal, reconnect state machine, health-gated two-slot update, resource watchdog | F5 AC-RT1, AC-RT8 | a node advertises only what it attested; an update that fails its health gate rolls back |
| Hook plus service-backstop probe matrix and tri-state receipts (PASS, FAIL, UNVERIFIED), with an evidence verifier that cannot pass vacuously | F6, F7, every Verify by | an enforcement claim without a backstop probe is UNVERIFIED, not PASS |
| Identifier taxonomy: a trace id is never authority; trace context accepted only from an explicit opt-in header | F1 AC-5, F11 AC-TR3 | no authorisation decision reads a trace id or a propagated header |
| Event envelope with a mandatory tenant, gap detection, explicit drops, monotonic versus wall time; a never-blocking bus and write-ahead log; a bundle checked with canary secrets and a hash manifest | F1, F11 | an explicit drop event for any loss; a canary-secret scan over every exported bundle |
| Effect broker with one-subject signed receipts, a hash-chained ledger and one-use operations | F8, F4, F11 | a receipt names one request hash and cannot be reused for another |
| Epoch-fenced leases and a signed acceptance that takes priority over database state | F5, F12 | a stale epoch cannot write; acceptance evidence outranks a mutable status column |
| Credential broker: origin taken from the live target, approval gating, read-back refusal, redaction | F4 AC-ID4, F8 AC-GW5 | a credential is only released for the origin the tool is really talking to; it cannot be read back |
| Adapter run loop: enrol, approve, poll, dispatch to an adapter, reply, acknowledge after the effect, dead-letter on restart | F6, F9 | acknowledgement follows the effect; a restart dead-letters unfinished work instead of replaying it blind |
| Per-session model proxy: billed cost of record, model allow-list; per-task cgroup reaping; a worker user | F10 AC-GA1, F5 AC-RT1 | the cost of record is the provider's billed figure; the model list is enforced at the proxy |
| Granted, expiring kill switches and an enforcement matrix per adapter | F4, F6, F7 | the presence of a kill switch proves nothing; an adapter declares what it can enforce |
| Fail-closed egress proxy, disposable execution cell, command filtering on the browser protocol | F8 AC-BR1, AC-CI2 | a browser or code session reaches only allow-listed hosts and commands |
| Hub with content-addressed storage, signed hub events and a restore drill | F3, F5, F9, F11 | Postgres row-level security instead of a file hub; a restore drill is part of the Verify by for any stored state |
| Envelope with canonical JSON, typed actor, causation parents and a two-layer reader that parks events of unknown kinds | F1 | tenant and trace ids added; the reader parks, never crashes (as F1a) |
| Approval permit field set (subject, scope, expiry, nonce, issuer), asymmetric signatures | F4 AC-ID6 | the permit names one request hash; single use |
| Identity chain: machine-nested agent principals, local attestation, revocation epochs | F4 AC-ID1, F5 | revoking a machine epoch invalidates its agents' tokens at once |
| Browser capability contract: session capability, control epoch, ownership modes, sensitive-session taint | F8 AC-BR1, AC-BR2 | a human takeover bumps the control epoch and fences the agent |
| Route matrix, effect gateway that rechecks at execution time, Rule-of-Two | F7, F8 | see AC-PL6 |
| Lease with fencing as one concept for election, device exclusivity and worktrees | F5, F8, F12 | one fence primitive, one test suite |
| Two-level Merkle ledger with crypto-shredding | F11 AC-TR2, F14 AC-ME6 | per-tenant erasure keeps the chain verifiable |
| Resident-engine adapter contract with capability negotiation and a quirk table | F6 | fail fast on a capability mismatch; one enforcement tier declared per adapter (see AC-PL7) |
| Epoch-fenced reconciler with a prepared-then-read-postcondition crash protocol and agent words as hints | F5, F12 | a crash between prepare and commit is settled by reading the postcondition |
| Effector boundary: typed request, parse and confine, default-deny gate, a broker that holds all credentials, safe errors | F7, F8, F4 | justification text is never policy input |
| Reserve, provision, refund with idempotency and a TTL | F10 AC-CO1, F8 leases | the budget primitive for 429s and leases; replaces check-then-act caps |
| Desired-versus-observed reconciliation with SKIP LOCKED claims, fencing generation and atomic reservation | F5 | leaderless placement and lost-node handling |
| Per-tenant signed, hash-chained trace with an offline verification bundle and quarantine | F11 | an unkeyed chain over payload hashes is not enough: the chain is keyed or anchored |
| Event envelope with a per-stream sequence, durable timers, no unbounded in-memory index | F1, F9 | schema version and tenant mandatory |
| Admission pipeline gate, transform, observe, with a side-effect-free `Evaluate` and a recorded rationale; three enforcement tiers per adapter | F7 | the decision records what could really be enforced (an allow-list flag in a harness is advisory, not enforcement; the tier names are advisory, hook and enforced) |
| Lifecycle state machines with mandatory evidence, owned by the kernel | F2 | per-stream-kind transition tables; evidence is a digest-stamped artifact |
| Execution-environment interface: provision, exec, copy in, copy out, teardown, cost, optional snapshot | F8 AC-CI1, F5 | one sandbox provider contract for code, browser and desktop |
| Digest-pinned, versioned skill discovery with progressive disclosure | F16 AC-AD3, F8 registry | archive safety, skip-unknown semantics, signed approval and per-tenant allow-lists |
| Anti-hollowness process: every slice's Verify by must be able to fail | all slices | already the rule here (failing control first) |
| Scrubbed-environment launcher for harness adapters | F6 | adapters never inherit the caller's environment (already enforced in the merged adapters) |

Not carried over: pooled third-party accounts; legacy supervision and release scripts; private evidence trees; hard-coded hosts.

Gaps in the first draft that the mining surfaced, now acceptance criteria:

- AC-PL6 (F7): taint tracking and a Rule-of-Two: a session that has read untrusted content and holds sensitive data cannot also take an outward effect without human approval; data read from a sensitive session taints later effects. Verify by: a scripted session in which untrusted tool output plus a secret read blocks the next outward effect; clearing requires a recorded approval. NEG: a taint label cannot be dropped by a tool the model controls.
- AC-PL7 (F7): fail-closed rollout practice: a new rule runs in a degrade log (decisions recorded, nothing blocked) before enforcement, and enforcement requires the log to show the expected decisions; each adapter's descriptor records the enforcement tier Fabric assigned (advisory, hook, or enforced, where enforced means a boundary Fabric holds, such as the sandbox or the broker) and the decision record names it. Verify by: a bundle in log-only mode records decisions and blocks nothing; promoting it to enforce without log evidence is refused; a claim of enforcement without a backstop probe is UNVERIFIED.
- AC-BR4 (F8): human presence and override: a human can pause, take over and return control of a browser or desktop session; the agent cannot act during takeover; each change bumps the control epoch. Verify by: an epoch test with a stale agent write refused.
- AC-CO5 (F10): honest kill-switch reporting: a kill switch stops new work immediately and reports a running command as still running until it has really ended; it never reports a running command as stopped. Verify by: a long-running command, kill, status shows running until the process group is gone.
- AC-KB8 (F14): the knowledge base answers "unknown" when retrieval has no supporting source rather than generating one. Verify by: a question with no matching source returns unknown with no citation.

Cautions carried with these inputs: a posture of "consent once, then full power, no prompts" is not adopted, only its mechanisms (signed manifests, revocation, audit), because Fabric's policy is configurable (F7); a trace may store content, so stored records use digest-and-reference; a known gap of receipts lost when control is down means the F1 stream needs a durable local outbox on the node; private evidence trees and hard-coded production addresses are not imported.

## Microsoft and Google crosswalk

Requirement (goodqa#15361, owner message of 2026-11-08): map the Microsoft agent stack (Agent Framework, Azure AI Foundry Agent Service, Foundry evaluations, tracing, content safety and guardrails, Entra Agent ID, A2A and MCP support) and the Google agent stack (Agent Runtime, formerly Vertex AI Agent Engine, with Sessions, Memory Bank, code execution, evaluation and tracing; ADK; Agent Builder and Agent Garden; A2A; Gemini Enterprise) onto Fabric, add what is missing, give Fabric its own first-party implementation of every component, and let a tenant or user replace or mix providers per component through versioned interfaces.

The AWS matrix above stays the row-level reference. The table below is the same map one level up: each Fabric component against the three providers, so a reader sees where Microsoft and Google differ from AWS. Row ids refer to the AWS matrix; MS-n and GO-n rows (next section) hold what only Microsoft or Google offers. Status is Fabric's own summary of the component, not a rollup: PARTIAL means a real piece exists even where some of the listed AWS rows are GAP, so read the row ids for the detail. Sources are in the Microsoft and Google table under Sources.

| Component (provider interface) | Fabric first-party and slice | AWS AgentCore (rows) | Microsoft | Google |
|---|---|---|---|---|
| Runtime (`runtime`) | PARTIAL: native, claude-code adapters, per-run sandbox; F5, F6, F16 | Runtime, Harness (RT-1 to RT-18) | Prompt agents, hosted agents (VM-isolated session, persistent home, scale to zero, Responses and A2A protocols), Agent Framework | Agent Runtime (container, ADK and framework templates, VPC-SC, streaming), ADK, Managed Agents API (preview) |
| Sessions and memory (`memory`) | GAP: event log only; F14 | Memory (ME-1 to ME-7) | Foundry conversations, per-user durable state store, Foundry Memory (preview), Agent Framework context providers | Sessions, Memory Bank (profiles, revisions), ADK sessions |
| Tools and gateway (`tools`) | GAP: F8, F10 | Gateway, Registry (GW-1 to GW-12) | Toolbox (one MCP endpoint, tool search, skills), tool catalog, MCP and OpenAPI tools, API Management AI Gateway | Agent Gateway (ingress and egress, default-deny), Agent Registry, ADK tools, Google MCP servers |
| Code sandbox (`sandbox`) | PARTIAL: Landlock, cgroup, Seatbelt controls; F8, F5 | Code Interpreter (CI-1 to CI-4) | Code interpreter tool, hosted-agent sandbox | Code execution sandbox (Python, no network), shell and custom-container sandboxes |
| Browser and desktop (`computer`) | GAP: F8 | Browser (BR-1 to BR-4) | Browser automation (live view, take control, recording), computer use | Computer Use sandbox (CDP, live view) |
| Identity (`identity`) | GAP: actors are self-asserted; F4 | Identity (ID-1 to ID-8) | Entra Agent ID (blueprint and instance, on-behalf-of, conditional access, sponsors) | Agent Identity (SPIFFE, certificate-bound tokens), context-aware access |
| Policy (`policy`) | PARTIAL: review gate in shadow, evaluate and enforce modes; F7 | Policy (PO-1 to PO-5) | Guardrail intervention points, egress controls | Governance policies (IAM, natural-language constraints), Agent Gateway enforcement |
| Guardrails (`guardrail`) | PARTIAL: secret and literal redaction only; F7, F10 | Guardrails (GR-1 to GR-12) | Foundry guardrails (content, prompt attacks, protected material, PII, task adherence, groundedness) | Model Armor (prompt injection, harmful content, sensitive data, malware) |
| Evaluation (`eval`) | GAP: verification before success only; F15 | Evaluations, Optimization (EV-1 to EV-4, OP-1 to OP-3, BE-1 to BE-5) | Built-in agent evaluators, AI Red Teaming Agent, continuous evaluation, agent optimizer | Agent evaluation, user simulation, online monitors, Optimizer, ADK eval |
| Observability and trace (`trace`) | PARTIAL: harness JSONL trace, metrics; F11 | Observability (OB-1 to OB-6) | OpenTelemetry tracing with multi-agent conventions, sensitive-content table | Cloud Trace, Logging and Monitoring via OpenTelemetry |
| Model gateway (`model`) | PARTIAL: native loop speaks Anthropic only; F10 | Gateway model routing (GW-6, RT-9) | Model catalog, model router, AI Gateway quotas | Model Garden, ADK model routing |
| Knowledge and retrieval (`knowledge`) | GAP: F14 | Knowledge Bases (KB-1 to KB-8) | Foundry IQ (permission-aware retrieval), file search, AI Search, Bing grounding | RAG Engine, Vector Search, grounding |
| Multi-agent and A2A (`agents`) | GAP: F9, F12 | Multi-agent (RT-18, AG-3, GW-8) | Agent Framework workflows, A2A v1.0 endpoint and tool, connected agents (unverified) | ADK multi-agent and graph workflows, A2A v1.0 |
| Registry and catalog (`registry`) | GAP: F8, F16 | Registry (GW-11, GW-12) | Entra Agent Registry, Foundry portal | Agent Registry, Agent Garden, Agent Studio |
| Cost and payments (`cost`, `payments`) | PARTIAL: native spend caps; F10, F8 | Cost, Payments (CO-1 to CO-5, PY-1, PY-2) | quota panes, per-agent cost view; no payments | gateway token budgets (announced), AP2 payment mandates |

### Rows only Microsoft or Google offers

Same status vocabulary and row format as the AWS matrix. A capability that an AWS row already covers is not repeated: for example live view and human takeover (BR-2), guardrails on tool calls (GR-9), memory namespaces (ME-5) and online evaluation (EV-3) cover the Microsoft and Google equivalents. A new `AC-XPn` is added only where the capability is genuinely absent. A2A v1.0 (endpoint, tool, agent card, task states, push), AP2 payment mandates and a computer-use sandbox over CDP are not repeated as rows: they extend RT-7 and GW-8 (F9 AC-MB4), PY-1 (F8 AC-PY1, gated) and BR-1 and BR-2 (F8).

| ID | Provider capability | Status | Fabric evidence | Slice and AC |
|---|---|---|---|---|
| MS-1 | Toolbox: a versioned tool bundle behind one MCP endpoint, promote-to-default, tool search, skills as versioned resources, usable by runtimes outside the platform | GAP | none | F8 AC-XP1 |
| MS-2 | Agent identity governance: a human sponsor, expiry and conditional-access context on every agent identity | GAP | none | F4 AC-XP2 |
| MS-3 | Per-destination egress allow-list for a hosted agent | PARTIAL | `internal/egress` enforces a host:port allow-list with an SNI match in the Linux sandbox (see AC-CI2); not macOS, not for tools outside the sandbox | F7 AC-XP3 |
| MS-4 | Automated agent red teaming (prohibited actions, data leakage, task adherence, indirect prompt injection) reporting an attack success rate | GAP | none | F15 AC-XP4 |
| MS-5 | Sensitive trace content routed to a separate, access-restricted store | GAP | secret and literal redaction only | F11 AC-XP5 |
| MS-6 | Retrieval under the caller's identity with source ACLs honoured | GAP | none | F14 AC-XP6 |
| MS-7 | Multi-agent trace conventions: spans named `invoke_agent`, `invoke_workflow`, `plan`, `execute_tool` | GAP | none | F11 AC-XP7 |
| MS-8 | Voice agents and telephone channels | GAP | none | none: owner decision 6 below |
| MS-9 | Workflow with durable checkpoints, human-in-the-loop, fan-out and fan-in (Agent Framework) | PARTIAL | reducer and fences are durable; no declarative workflow | F16 AC-FL1, F12 AC-MA1 |
| GO-1 | One ingress and egress enforcement plane, default-deny, routing only to registered agents and tools | GAP | none | F8 AC-XP8 |
| GO-2 | Semantic governance: a natural-language constraint checked against a tool call's intent at run time | GAP | none | F7 AC-XP9 |
| GO-3 | Sender-constrained agent credentials: a short-lived workload certificate and tokens bound to the agent's key | GAP | none | F4 AC-XP10 |
| GO-4 | Memory with immutable revisions, a rollback window and schema-filled profiles | GAP | none | F14 AC-XP11 |
| GO-5 | A gallery of sample agents that run unchanged | GAP | none | F16 AC-XP12 |
| GO-6 | Visual agent builder (Agent Studio, Agent Designer) | GAP | none | out of Fabric: the UI belongs to GoodQA; Fabric exposes the agent definition API (F16 AC-AD1) |
| GO-7 | Managed store of few-shot examples (Example Store; source details unread, low confidence) | GAP | none | none: confirm need before any AC |

### Cross-provider additions (new acceptance criteria)

- AC-XP1 (F8, extends AC-GW7 and AC-AD3): a tool bundle has immutable versions and a promoted default; one MCP endpoint serves the default (or a pinned version) of a bundle, with auth and guardrails applied at the endpoint; skills are served as versioned resources. Verify by: `go test ./internal/tools/bundle`: publish v1 and v2, promote v2, a client pinned to v1 keeps v1, rollback restores v1. NEG: a client cannot list a bundle version it has no grant for.
- AC-XP2 (F4): every agent identity has a human sponsor and an expiry; an expired or sponsorless identity is refused on its next command; context conditions (node, network zone, time) can deny. Verify by: identity tests with an expired identity, a removed sponsor and a denied context. NEG: extending an expiry needs a principal other than the agent.
- AC-XP3 (F7): extends AC-CI2 (the allow-list and SNI match that exist on Linux) to the macOS sandbox and to broker-level enforcement for tools that run outside the sandbox, with every refusal traced. Verify by: the same destination test on macOS, and a broker-run tool refused for a host not on the list. NEG: a DNS name that resolves to an allowed address is not itself allowed.
- AC-XP4 (F15): an automated red-team run replays attack categories (prohibited action, data leakage, task adherence, indirect prompt injection) against an agent definition in a disposable environment and reports the attack success rate per category. Verify by: a run against a deliberately vulnerable fixture agent reports a non-zero rate, and against a hardened fixture a zero rate; the run uses only the provider named in the grant. NEG: the run cannot write outside its disposable environment.
- AC-XP5 (F11): fields classed sensitive are written to a separate store with its own role; the shared trace carries a reference and a digest. Verify by: a test that the shared trace role cannot read the sensitive store and a dump of the shared trace contains no sensitive value. NEG: removing the class tag does not move existing records back into the shared trace.
- AC-XP6 (F14): a retrieval carries the caller's identity and returns only chunks whose source ACL allows that identity; an ACL change takes effect on the next query. Verify by: two principals, one document, one allowed; revoke and query again. NEG: an agent's own identity never widens the user's access.
- AC-XP7 (F11): spans use the names `invoke_agent`, `invoke_workflow`, `plan` and `execute_tool` with the OpenTelemetry generative-AI attributes, and a child agent's span links to the parent. Verify by: a two-agent run exported to a fixture collector, checked against fixed vectors. NEG: a span without a trace id is not exported.
- AC-XP8 (F8, extends AC-GW7 and AC-PL1): the gateway routes a request only to a registered tool or agent, and its default is deny for ingress and egress; an unregistered target is refused and traced. Verify by: a registered and an unregistered MCP server and A2A peer. NEG: a registry entry that is withdrawn stops routing on the next request.
- AC-XP9 (F7): a policy may carry a natural-language constraint that a model-backed judge checks against each tool call's stated intent; it starts in shadow mode. In enforce mode a judge outage or timeout denies the call or asks for approval, as AC-PL1 requires, and the outage is recorded; in shadow mode it only records. Verify by: fixtures of intent-matching and intent-violating calls, a judge outage in each mode, and mode transitions recorded. NEG: the judge's output cannot change the policy bundle digest or add an allow rule (a test that runs the judge with an adversarial response and compares the digest before and after).
- AC-XP10 (F4): an agent credential is short-lived and bound to a key the agent holds; a token used from a different key is refused. Verify by: a test with a stolen token replayed from another key, and an expired certificate. NEG: a revoked identity's still-unexpired token is refused at the broker.
- AC-XP11 (F14): every memory write creates an immutable revision; a deleted or overwritten record can be restored within a stated window; a profile schema constrains the fields an extraction may fill. Verify by: write, overwrite, delete, restore tests and a schema-violating extraction. NEG: a restore cannot cross tenants.
- AC-XP12 (F16): a gallery of sample agent definitions ships in the repository and each runs under `fab smoke --parity` with first-party providers only. Verify by: the smoke run. NEG: a sample that needs a network service other than the configured model provider fails the check.

## Provider interfaces (epic constraint)

This is a design constraint on every slice and an acceptance criterion for the program. The decision is recorded in [ADR 0002](adr/0002-provider-interfaces.md).

1. **First party.** Fabric ships its own implementation of every component in the crosswalk: runtime, sessions and memory, tools and gateway, code sandbox, browser and desktop, identity, policy, guardrails, evaluation, trace, model gateway, knowledge and retrieval, multi-agent and A2A, registry, cost. A kind whose use is gated by an owner decision (payments, AC-PY1) is exempt until that decision. A fresh install uses only these (AC-OOTB).
2. **A versioned interface per component.** Each kind has a contract named `fabric.provider/<kind>/v1` (the table above gives the kinds), written as Go interfaces plus a wire form for out-of-process providers, with a golden descriptor and a conformance suite.
3. **A descriptor.** Each adapter declares: kind, name, contract version, capabilities, data residency, network need and spend class; the descriptor also records the enforcement tier that Fabric assigned. The tier names are those of AC-PL7: `enforced` (a boundary Fabric holds, the sandbox or the broker, stops the action whatever the provider does), `hook` (the provider calls back into Fabric's decision before it acts, and a probe checks that it does) and `advisory` (the provider's own checks, which Fabric only records). The tier is assigned by Fabric from where enforcement really happens and verified by the conformance probe; a self-declared tier is a claim, not a fact. A provider-hosted runtime counts as `advisory` for every call it makes inside the vendor, and Fabric's events cover only the boundary.
4. **Per-tenant and per-stream binding.** A tenant chooses a provider per kind; a stream may override where the kind allows it. A run pins its bindings at start, so a binding change never applies mid-run.
5. **One policy, one trace, never unbound.** Fabric's own policy decision point (F7) and event log (F11) cannot be replaced. A third-party policy provider adds checks: a deny from either wins and it can never turn a Fabric deny into an allow. A third-party trace provider is an export sink only; the hash chain and audit stay Fabric's. The decision and the record are made around every provider call, whichever provider is bound, so a provider cannot skip them. A kind maps to the capability classes it serves (`sandbox` to `shell` and `fs.*`, `computer` to `browser.*` and `desktop.*`, `model` to `llm.call`, `tools` to `net` and `ticket.*`); policy names classes, bindings name kinds. `message.send` is served by the `agents` kind and `git.publish` by `tools`. A `runtime` or `agents` provider that executes calls Fabric cannot mediate is treated as serving every class it can reach, so it cannot be bound to a tenant or stream whose policy marks any such class must-enforce.
6. **Mixing.** Providers combine freely across kinds, for example Bedrock models with Foundry evaluations and Fabric memory.
7. **Vendor adapters.** AWS, Microsoft and Google adapters are interface plus contract tests against a fake first. A live adapter that spends money needs a recorded decision before it can be enabled: for a tenant's own cloud account, the tenant administrator's signed event; for an adapter Fabric pays for, the Fabric owner's. A Google adapter is never run live: Fabric's deployments, CI and tests never call Vertex or `aiplatform`, hold no `aiplatform` IAM grant and run no live Vertex adapter (enforced at run time and by a static check, AC-PI8).

New acceptance criteria (slice F17, below):

- AC-PI1 (F17): every kind has a versioned contract with a golden descriptor; an unknown kind, version or capability name is refused at registration. Verify by: `go test ./internal/provider/registry`. NEG: a descriptor that claims a capability its conformance run did not exercise is refused.
- AC-PI2 (F17): every kind has a first-party implementation (except a kind gated by an owner decision, payments) and `fab smoke --parity` passes with only first-party providers. Verify by: the smoke run on a fresh install. NEG: unbinding the first-party implementation of a kind without a replacement fails startup.
- AC-PI3 (F17): each kind has a conformance suite that a provider runs against itself and that can run as a probe against a live adapter. Verify by: the suite passes for the first-party provider and for the fake third-party provider, and fails for a mutated provider (a mutant per kind). NEG: a suite that passes for a provider returning empty results is rejected.
- AC-PI4 (F17): enforcement tiers are verified, not declared. A capability class that policy marks must-enforce can be served only by an `enforced` provider; `hook` and `advisory` providers are refused for it. Verify by: a policy that requires `enforced` for `shell` against fake providers declaring `hook` and `advisory` (both refused), and a conformance probe that fails a provider whose declared tier is higher than its measured one. NEG: a provider that upgrades its tier at run time is refused; a provider-hosted runtime is classed `advisory` for the calls it makes inside the vendor, and binding a fake hosted runtime to a stream whose policy requires `enforced` for `shell` is refused.
- AC-PI5 (F17): bindings are per tenant and per stream, pinned per run. Verify by: tenant A binds a fake provider, tenant B sees the first-party one; a binding changed during a run applies only to the next run. NEG: tenant B cannot read tenant A's binding or credentials.
- AC-PI6 (F17): policy and trace are provider-independent and cannot be unbound. Verify by: the same mission run once with first-party and once with fake third-party providers yields the same policy decisions and trace event kinds; a fake third-party policy provider that returns allow for a call Fabric's own policy denies is overridden (deny wins); a fake trace provider is only an export sink, and the stored events and chain of one run are identical with and without the sink bound. NEG: a provider that returns without Fabric having written the intent and result events fails the call, and unbinding the first-party policy decision point or event log is refused at registration.
- AC-PI7 (F17): providers can be mixed across kinds in one mission. Verify by: a mission with model, evaluation and memory each bound to a different fake provider completes with one trace. NEG: a provider bound for one kind cannot be called for another.
- AC-PI8 (F17): vendor adapters exist only as interface plus a contract test against a fake until a decision is recorded, and a Google adapter has no live path. Enforced at run time first: the egress mediator and the model gateway deny the Google AI Platform and Agent Platform API hosts (`aiplatform.googleapis.com`, the regional `*-aiplatform.googleapis.com`, and the Agent Platform API hostnames named in Google's release notes, which are listed in the deny-list before this criterion can pass), the gateway refuses `vertex_ai/` style model ids and an Anthropic-on-Vertex route, and the worker environment drops `GOOGLE_GENAI_USE_VERTEXAI` and `GOOGLE_APPLICATION_CREDENTIALS`. Then a static check over code, workflows and infrastructure files (documentation excluded by path) fails on an `aiplatform` host, a Vertex client library or an `aiplatform` IAM grant. Verify by: egress and gateway tests for each refusal, a registry test that refuses to enable a spending live adapter without a decision record, and a CI test with a planted forbidden reference that the check catches. NEG: a decision record written by hand into a tenant's configuration does not enable it, and an allow-list entry for the static check needs the owner's recorded decision. ASSUMPTION (inferred, not the owner's words): the Gemini Developer API is not Vertex and is outside this rule; whether any Google model provider is wanted is owner decision 5.
- AC-PI9 (F17): a third-party or user adapter runs out of process over a documented transport with least privilege and credentials from the credential provider. Verify by: an adapter that crashes, hangs or returns malformed data fails closed within a bound and the run reports the provider by name. NEG: an adapter process cannot read another adapter's credentials.
- AC-PI10 (F17, extends AC-ME6): data a provider holds can be exported in a neutral, versioned form (memory records, evaluation datasets and results, traces) so a tenant can leave a provider. Verify by: export from one fake provider and import into another, byte-compared on the neutral form. NEG: an export never contains another tenant's records.
- AC-PI11 (F17): the descriptor's capability list and tier are written into every binding event and every provider-call record, so an audit shows which provider served which call at which tier. Verify by: a trace of a mixed mission lists provider name, contract version and tier on each call. NEG: a call without that record is refused.

## Slice addition: F17 Provider interfaces

Dimensions: multi-harness, multi-machine, configurable policy, traceable, multi-tenant. Fixes one irreversible choice, the provider contract names and descriptor fields (ADR 0002). It depends on F3 (tenant binding) and F7 and F11 (policy and trace around every call). Every slice that owns a kind (F4 identity, F5 runtime and sandbox, F6 runtime, F7 policy and guardrails, F8 tools, sandbox and browser, F9 and F12 multi-agent, F10 model and cost, F11 trace, F14 memory and knowledge, F15 evaluation, F16 registry and agents) builds its first-party implementation behind the contract for its kind, so the contracts are drafted early. Its acceptance criteria are AC-PI1 to AC-PI11 above; they build on the per-component interface criteria AC-GU2, AC-KB3, AC-RT1 and AC-RT5.

## Out of the box on a fresh personal install

AC-OOTB (F13): on a fresh personal install (one binary plus the bundled or managed Postgres, local issuer, no cloud account and no network access beyond the model provider the user configures), `fab init` followed by `fab smoke --parity` runs one check per row group above and every check must pass or fail visibly; none may be skipped silently. Verify by: a scripted install on a disposable host in CI. Specifically:

- policy defaults are on (deny by default for unknown capability classes), the guardrail local provider runs with default content, PII and prompt-attack settings, and traces and metrics are recorded without configuration;
- identity works through the local issuer; the token vault, memory store, knowledge base and registry are created by `fab init` in the same database;
- a local tool gateway and a local MCP fixture are registered, and the code capability and the browser capability run, and on the reference CI host every group must PASS (on a host missing a prerequisite the smoke test reports that group as FAILED with the exact prerequisite, never as passed);
- every provider kind has a first-party implementation bound by default (AC-PI2), and the smoke run lists the bound provider for each kind;
- no group requires a hosted AWS, GCP or other cloud service; a smoke run with all outbound network except the configured model provider blocked still passes;
- NEG: a smoke run with the guardrail provider disabled by configuration fails the smoke test (the default is not allowed to be silently off).

## Decisions needed from the owner

1. Payments (AC-PY1): real-money capability. Recommendation: spec it separately, build only after an explicit product decision; not part of out-of-the-box.
2. Policy language (AC-PL2): Fabric JSON bundles first with a Cedar importer later, or Cedar from the start.
3. Guardrail provider: the built-in local provider is rule and classifier based and model-agnostic. Confirm that a model-backed classifier through the gateway is acceptable for prompt-attack and grounding checks, and which model class (a local small model or any configured provider).
4. Knowledge-base store: Postgres vector extension by default for the personal install. Confirm.
5. Provider adapters (AC-PI8): confirm that a live AWS or Microsoft adapter needs a recorded decision for spend (tenant administrator for a tenant's own account, Fabric owner otherwise) before it is enabled, that a Google adapter stays interface plus fake only (Fabric never calls Vertex or `aiplatform`), and whether any Google model provider through the Gemini Developer API is wanted at all.
6. Voice agents and telephone channels (MS-8) and a managed example store (GO-7): add them to scope or leave them out. Recommendation: leave both out until a product need is stated.
7. Order: F4 (identity) and F7 (policy and guardrails) first because everything else is gated by them; F14 to F16 after F1 to F3 settle the envelope, streams and tenancy. F17 (provider contracts) is drafted with F6 and F8, before the other slices ship their first-party implementations, and its contracts are reviewed with F7 and F11.

## Check

    for s in HAVE PARTIAL GAP; do grep -E '^\| [A-Z]{2}-[0-9]+ \|' docs/aws-agent-parity.md | grep -c "| $s |"; done
