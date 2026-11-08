# ADR 0001: Fabric is the tenant-aware execution and trace kernel

- Status: accepted (2026-11-08)
- Ticket: goodqa#15361 (program), slice F0
- Supersedes: the README framing "a small control plane for running bounded, disposable coding-agent workers"

## Context

Fabric today is a careful durable-execution core with a narrow shape. The audit of `main` at `9abc522` found that the code assumes:

- a mission must be a GoodQA ticket (`missions.ticket NOT NULL`), and the mission is the only aggregate;
- the lifecycle is a coding pipeline and work is one writer plus one reviewer;
- the provider enum is closed (`claude-code`, `anthropic-api`) and launches are headless one-shot `claude -p`;
- a machine id is its hostname, there is one scheduler per host and one global `slots` row;
- events are a flat, unversioned union struct, and actors and approvers are self-asserted;
- spend caps are constants, egress has one destination, and policy is inverted (Fabric calls the harness hooks);
- there is no `tenant_id`, and no browser, desktop or messaging capability.

Most of the other capabilities we want already exist in pieces elsewhere: tenancy, users, machines and tickets in GoodQA; interactive Claude and Codex adapters in the harness console; per-host runners; an isolated browser engine. None of them has Fabric's rigour: a pure reducer, fences, database-time leases and effect readback. Hardening the single-host slice further would deepen the narrow shape. This ADR fixes the target and the order of work instead.

## Decision

1. **Role.** Fabric is the multi-tenant execution and trace kernel. GoodQA is the organisation control plane: orgs, users, tickets and UI. Fabric takes identity from an issuer: GoodQA user tokens (or OIDC) in org mode, a `local` issuer in the personal edition. Work tracking stays in GoodQA (or a local ledger in personal mode); Fabric streams carry typed `external_refs`, not a mandatory ticket.
2. **One node agent.** The per-machine runtimes merge into one `fab-node` with harness adapters: `native` (the built-in loop), `claude-code` (interactive stream-json, not `-p`) and `codex` (ported from the console's Codex client). Later harnesses plug into the same interface. The console becomes a UI over Fabric's HTTP API instead of rebuilding its own event log. The legacy per-host runners retire after the node agent (F5) and adapters (F6) land.
3. **Kernel shape.** The event store holds typed streams (`stream_kind`, `stream_id`) with one reducer per kind, keeping the reducer discipline, mutants, outbox, fences and database-time leases. Today's mission becomes stream kind `work_run`.
4. **Capabilities, not hooks.** Every outside-world action is an invocation `{class, subject, scope, grant}`. Fabric records grants, leases, decisions and audit; the provider (browser engine, desktop agent, sandbox) enforces. Observations such as screenshots are trace artifacts with digests, not effects. Harness hooks call a Fabric decision point, not the reverse.
5. **Tenancy.** One shared database, `tenant_id` in every key and unique index, Postgres row-level security. Not a database per tenant. Postgres stays in the personal edition too (managed or bundled): fences, database-time leases and RLS depend on it.
6. **Sequencing.** Features run in parallel with the foundation. Foundation-first was considered and declined by the owner. Later migrations are an accepted cost, so feature work must keep them cheap (see Interim rules).
7. **Shadow-only exit is per capability.** Leaving shadow-only is not gated on F1 to F4. Each slice leaves shadow-only when its own live acceptance passes on the deployed build. Real work may run on live before the foundation lands, but only through the interim-rule shapes below: `tenant_id` leading the key, versioned nested payloads, and no fake-mission aggregates.

## The twelve target dimensions

Multi-harness including a built-in harness; multi-machine; multi-agent; configurable policy; traceable; long-term work tracking; full computer use; full browser use; inter-agent and inter-harness messaging; gateway trace and control; multi-tenant; multi-user and org-controlled, plus a personal deployment. The slice plan is in [../status-and-roadmap.md](../status-and-roadmap.md) and the per-slice spec outlines are in [../slices.md](../slices.md).

## Irreversible choices

These are decided once, before data accumulates. The slice that fixes each one is named; its spec review is the place to challenge it.

| # | Choice | Direction | Fixed by |
|---|---|---|---|
| 1 | Event envelope fields and canonical bytes for hashing | Versioned envelope; canonical form is RFC 8785 JSON canonicalisation of the envelope, independent of Go field order; typed payload per kind | F1 |
| 2 | Id shape | Kind prefix plus ULID body, at most 64 bytes. Machine ids are enrolled (`n_<ulid>`); a hostname is only a label | F1, F5 |
| 3 | Tenant key | `tenant_id` leads every primary key, unique index and idempotency scope. RLS on the application and runner roles | F3 |
| 4 | Stream generalisation | `stream_kind` plus `stream_id`; command keys per stream; the temporary ladder rows migrate to `stream_kind=ladder` | F2 |
| 5 | Hash chain | Per-stream chain over the canonical envelope bytes. F1 reserves nullable columns and the byte form; F11 turns it on from a recorded anchor | F1, F11 |
| 6 | Identity authority | GoodQA as issuer for orgs, `local` for personal. Principal and issuer on every event, `on_behalf_of` optional | F4 |
| 7 | Capability class names | `fs.*`, `shell`, `net`, `llm.call`, `browser.{open,navigate,act,read}`, `desktop.{observe,act}`, `message.send`, `ticket.*`, `git.publish`. Policy bundles and audit reference these names for good | F8 (names), F7 (use) |

## Interim rules for parallel feature work

Until F1 to F3 land, feature slices follow these rules so that moving them later is a mechanical migration:

- Every new table has `tenant_id` (defaulting to the personal tenant) as the leading key column, and nullable `stream_kind` and `stream_id` columns where the row belongs to a stream.
- Every new event or record kind uses a typed, versioned payload in one nested key (the pattern the ladder receipts use), never new top-level fields on the flat event struct.
- No new fake-mission aggregates. If a feature needs a stream and F2 has not landed, it keeps its state in its own table behind an interface and leaves the store coupling to a thin sink that F1 and F2 swap.
- Ids use the kind-prefix plus ULID shape from the start.
- Capability class names are used verbatim from the table above.
- A binary rollback must stay safe: `refuseNewerSchema` keeps working, and an envelope change ships with a minimum reader version.

## Consequences

- Some rows written before F1 to F3 (ladder receipts, missions) need migration and an upcaster. The ladder receipts are already marked temporary for this reason.
- The README and roadmap stop describing Fabric as a small coding-agent control plane. Claims stay limited to what is tested; the capability matrix and the honest-limits list remain.
- Retiring the legacy runners and rewiring the console are cross-repository work, tracked under F5, F6 and F11. They are not done by this ADR.
- GoodQA tables for daemons and launches become a view of Fabric nodes and runs only after F5 and F6; until then both exist.

## Alternatives rejected

- **Foundation first (F1 to F4, then features).** Safest for data, slowest to show value. Declined by the owner in favour of parallel lanes.
- **Keep Fabric shadow-only until F1 to F4 land.** Part of foundation-first: it holds every feature back from live use. Declined; exit is per capability, as in decision 7.
- **Fabric as the org control plane.** Duplicates users, orgs, tickets and UI that GoodQA already has.
- **Database per tenant.** Operationally heavy; fleet-wide sweeps, leader election and the shared scheduler get harder. Shared database plus RLS is enough, and the runner role is scoped by tenant and attempt.
- **SQLite for the personal edition.** Loses fences, database-time leases and RLS, which the kernel relies on.
- **Keep three runtimes and bridge them.** Keeps three event logs and three policy paths. One node agent with adapters is smaller.

## Verification

This ADR is checked by review. Each slice's own spec carries `Verify by:` checks. F0 is done when the ADR, README and roadmap are in `main` and the README scope line no longer says "small control plane".
