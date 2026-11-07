# Threat model

Scope: agent-fabric at commit `bc5a6d3`. This is a model of what the system defends, against
whom, and where the defence is. Every mitigation points to a gate in
[review-gates.md](review-gates.md) (R1 to R25, D1 to D8) and every open hole to a gap there
(G-1 to G-30). A mitigation with no gate reference is not claimed.

The model is STRIDE-shaped (Spoofing, Tampering, Repudiation, Information disclosure,
Denial of service, Elevation of privilege). It does not replace the gate document; it
shows coverage and the blind spots.

## Assets

| Asset | Why it matters | Where it lives |
|---|---|---|
| Mission state and event log | Source of truth for what ran, what was accepted, what is done | PostgreSQL: `events`, `commands`, `missions`, `attempts` |
| Effect records and their audit | The only record of whether an outside action happened | `effects`, `effect_dispatches`, `effect_resolutions` |
| Remote systems the broker writes to | Ticket tracker, git remotes | Reached only by executors in the broker |
| Provider API key and other credentials | Spend money, publish code | Key command output (in memory), key file (mode 0600), runner DSN in the worker environment |
| Spend budget | Direct money loss | Reservations and usage receipts (`reservations`, `usage_receipts`) |
| Host resources and the user's files | Integrity of the machine the workers run on | Host profiles (Seatbelt, Landlock, cgroups) |
| Worktrees and code under change | The deliverable | Per-run worktree and run directory |

## Actors

| Actor | Trust | Capability |
|---|---|---|
| Operator (human) | Trusted, but not authenticated by Fabric | Runs `fab` with `FABRIC_ACTOR=op:<name>`; owns the database role for `fab` |
| Scheduler/daemon (`fabd`) | Trusted code | App role; launches workers; writes scheduler events |
| Worker (agent process) | **Untrusted** | Runs under a host profile; may hold the runner DSN; may emit any text |
| Native loop | Trusted code, driven by an **untrusted** model | Calls the provider, runs confined tools, holds the provider key in memory. It writes events and reservations, which the runner role cannot, so it connects with the **app** role (G-28): the model-driven process holds the role that approves effects and writes any event |
| Content the agent reads | **Untrusted** | Repository files, tool output, web pages, ticket text: can contain instructions |
| Remote systems | Semi-trusted | May lose or duplicate responses; may change state independently |
| Database owner/administrator | Fully trusted | Runs migrations; can disable triggers |
| Another process on the same host/UID | **Untrusted** | Can run `fab`, read files of the same UID |
| Network attacker | Untrusted | Reaches only what the host profile allows out; `/metrics` is loopback by default |

## Trust boundaries

1. **Worker to Fabric state.** A worker has at most the runner database role. Boundary controls: R4 fences, R18 roles and triggers, R14g, R17. Integrity is enforced by these; availability is open (G-25: a runner session can pin locks, lock tables and lift its own timeouts).
2. **Model to tools.** The model's output is never executed directly; tool calls pass the allow-list and the oracle (R23).
3. **Fabric to the outside world.** Only the effect broker writes outward (R14); the worker's own network access is limited only on Linux by TCP port rules (R17, G-4).
4. **Operator to Fabric.** Attribution by string, not authentication (R20, G-5).
5. **Fabric binary to database.** Roles separate scheduler/app from runner; the owner role is outside the boundary (G-9).
6. **Build to deployment.** Clean-clone build with a version check (D7); no provenance (SLSA row in review-gates.md).

## STRIDE coverage

Legend for status: **Mitigated** (a gate with a test), **Partial** (a gate with a stated limit), **Open** (a gap).

### Spoofing

| Threat | Mitigation (gate) | Status |
|---|---|---|
| A worker acts as a different attempt or mission | Fence per attempt in every command, plus a trigger on effects (R4, R14g) | Mitigated inside Fabric; the shared runner role can still address any running mission (R18, G-19) |
| A process pretends to be an operator | `op:<name>` prefix allow-list (R20) | **Open**: self-asserted (G-5) |
| A forged wake or ask answer | Wake accepted only for an answered ask of the same mission; answers need a grammatical source (R24) | Partial: the source grammar proves form, not truth |
| A forged job exit or transport receipt | Job writes reserved to the scheduler (R19); dispatch settle needs the dispatch number (R14g) | Partial: a runner can forge a receipt for its own live dispatch (G-19) |
| A forged claim of evidence | Claim must cite this attempt's recorded evidence for the current head (R12) | Partial: the evidence itself is asserted (G-15) |

### Tampering

| Threat | Mitigation (gate) | Status |
|---|---|---|
| Rewrite history in the log | Append-only triggers on `events`, `commands`, `effect_resolutions`; TRUNCATE rejected (R21, R14f) | Mitigated against non-owner roles; **Open** against the owner (G-9) |
| Change an approved request after approval | Approval bound to the request hash; hash recomputed before transport (R14a, R14b) | Mitigated |
| Move an effect to a state the broker would not | Outcome guard trigger for every role (R14g) | Mitigated |
| Replay a command with different content | Idempotency key plus canonical bytes hash (R21, R14a) | Mitigated |
| Run a changed migration or an older binary on a newer log | Checksum on migrations; foreign event kind refused (R21) | Mitigated |
| A stale writer overwrites a newer attempt | Fence check (R4) | Mitigated inside Fabric; not at the remote edge |
| Alter the spend ceiling | Constants in code; grants cannot raise them (R15) | Partial: rollback to an older binary restores older ceilings; a skewed clock gives a fresh month (G-27) |
| Tamper with the build | Clean-clone build, version check (D7) | Partial: no attestation (SLSA row) |

### Repudiation

| Threat | Mitigation (gate) | Status |
|---|---|---|
| An operator denies clearing a park or resolving an effect | Actor, reason and evidence in the event or `effect_resolutions`; `db_user`, `xact`, `at` stamped by trigger (R14f, R20) | Partial: the actor string is self-asserted; the DB role is real (G-5) |
| A worker denies an action | Per-attempt events with fence; stale attempts recorded in `stale_observations` (R4) | Mitigated for Fabric-visible actions |
| Slot cap changed without a trace | none | **Open** (G-12) |
| Approval attribution | `approved_by` | **Open**: unauthenticated string (G-2) |

### Information disclosure

| Threat | Mitigation (gate) | Status |
|---|---|---|
| The provider key leaks into a log, transcript or fixture | Single-token key command, stderr dropped; redactor with 19 shapes; fixtures that match a shape are not written (R16) | Partial: deny-list, not complete |
| A worker reads secret files | Seatbelt deny-read list; Landlock filesystem rules (R17) | Mitigated for listed paths on the profiles that were tested; other paths readable on macOS (G-18) |
| A worker reads other missions' rows | none | **Open**: the runner role can read every table (G-19) |
| Exfiltration over the network | TCP port limits on Linux only (R17) | **Open** (G-4) |
| Metrics/TUI leak data or become an injection vector | Loopback only, Host/Origin checks, labels are ids and enums, terminal text sanitised (R25) | Mitigated; `--allow-remote` has no authentication |
| Prompt injection steers the agent to disclose | Oracle on shell; confined tools (R23) | **Open** for taint tracking (gate angles) |

### Denial of service

| Threat | Mitigation (gate) | Status |
|---|---|---|
| Unbounded concurrent missions | Slot cap (R1) | Mitigated (count only) |
| Runaway spend | Run/job/month caps with hold-before-send (R15) | **Partial, bypassable until fixed** on the native path: holds of an ended attempt can be released uncounted (G-26) and the month key uses the process clock (G-27); the `claude -p` path allows USD 1000 per run and has no month ledger (G-28); not reconciled with the bill (G-17) |
| A looping or failing launch | Park after 6 prelaunch failures; backoff; capacity waits excluded (R8) | Mitigated; a database outage can park missions (fail closed) |
| A hung remote holds a connection | Dispatch deadline, slot limit, advisory lock (R14h) | Mitigated |
| One bad row stalls a sweep | Per-row isolation (R9) | Mitigated |
| Resource exhaustion by a worker | cgroup `MemoryMax`/`CPUQuota`/`TasksMax` on Linux (R17) | Mitigated on Linux; not on macOS |
| Slow metrics scrape locks the database | Read-only transaction with timeouts, one at a time (R25) | Mitigated |
| A worker never ends | Lease expiry to UNKNOWN; wall clock in the grant (R5) | Mitigated; reconcile needs the prober (R11) |
| A worker holding the runner DSN pins the broker's advisory lock, locks `effects`/`jobs`, lifts its own timeouts or exhausts connections | none effective: the 60 s timeouts are role defaults a session can override (R18) | **Open**, reproduced by the red team (G-25) |

### Elevation of privilege

| Threat | Mitigation (gate) | Status |
|---|---|---|
| A worker escapes its sandbox via the scheduler's job launch | Host jobs disabled; no INSERT; trigger (R19) | Mitigated |
| A worker starts a sibling process outside its confinement | Linux: dedicated worker account (repair in progress) | **Open** on same-UID hosts (G-8) |
| A worker approves its own effect | Runner role cannot write approval (R14b); the app role path self-approves (R14b Limits); the native loop itself holds the app role (G-28) | Partial (G-1, G-2, G-28) |
| A worker ends its own attempt or frees its hold | Runner role cannot write those tables (R18) | Mitigated |
| An unapproved effect of a kind that does not demand approval is sent | none per kind except `publish` | **Open** (G-1) |
| A shell command escapes the allow-list | Anchored allow-list, oracle fail-closed (R23) | Partial: the oracle's rules live outside the repo (G-20) |
| A push overwrites someone else's work | Head compare-and-swap, fast-forward only, bound destination, mirror, allow-listed env (R14i) | Mitigated for the publish executor |
| A caller reaches the database owner role | Roles never own tables; the owner is used only for migrations (R18) | Convention: how the owner DSN is handled is a deployment matter |

## What the model leaves out

- Side channels and kernel vulnerabilities (the sandbox is as strong as the kernel feature it uses).
- Compromise of the provider or of a tracked remote.
- Multi-tenant isolation (there is one logical tenant; see [gate angles](gate-angles.md)).
- Worker artifacts that are later run by trusted operator or CI tooling (build scripts, editor and environment configs) and poisoned repository content; both are listed as gate angles, not mitigated.
- Integrity of the external hook scripts the policy oracle calls (G-20).
- Insider threats at the database owner level (G-9).
- Supply-chain compromise of the Go toolchain or the two dependencies; no SBOM or provenance is produced.

## Review of this model

This model was reviewed by an independent adversarial pass (D4). Round 1 added the runner-role denial of service (G-25), the spend bypasses (G-26, G-27) and the native loop's role (G-28).
