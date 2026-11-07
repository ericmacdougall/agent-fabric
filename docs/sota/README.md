# State of the art, component by component

One note per major component: what the cited current practice is, how Fabric compares with
evidence (`file:line` at commit `bc5a6d3` and a test), and ranked upgrades. Sources are
dated 2026-11-07. A verdict is one of `ahead`, `parity`, `behind` or `different scope`;
`ahead` is used only when a cited source documents the peer doing less and Fabric's code and a
test do it. Where a peer's page is silent the note says `parity (peer not documented)`, because
silence is not proof of absence. The overall comparison and the gate list are in
[review-gates.md](../review-gates.md); the threat model is in [threat-model.md](../threat-model.md);
gate types Fabric lacks are in [gate-angles.md](../gate-angles.md).

| Component | Note | Fabric's weakest point here |
|---|---|---|
| Scheduling and admission | [scheduling-admission.md](scheduling-admission.md) | Priority-ordered queue with a batch limit; no fairness or quotas |
| Durable state and event sourcing | [durable-state-events.md](durable-state-events.md) | No versioning or growth policy for in-flight logs |
| Fencing and leases | [fencing-leases.md](fencing-leases.md) | The fence is not sent to remote systems; no clock-step test |
| Effect broker and reconciliation | [effect-broker.md](effect-broker.md) | Approval is optional per submission; no compensation |
| Sandboxing per OS | [sandboxing.md](sandboxing.md) | No egress policy; Linux same-UID gap; no Windows |
| Credential handling | [credentials.md](credentials.md) | Runner DSN in the worker environment; no per-run credential expiry |
| Native agent loop | [native-loop.md](native-loop.md) | No verifier before success; context management not found |
| Cost control | [cost-control.md](cost-control.md) | Caps bypassable under goodqa#15123 and goodqa#15124; not reconciled with the provider's bill |
| Observability and audit | [observability-audit.md](observability-audit.md) | No hash chain or export; no tracing |
| Operator UX | [operator-ux.md](operator-ux.md) | Operator identity is self-asserted |
| Multi-machine scheduling | [multi-machine.md](multi-machine.md) | Single host |
| Evals | [evals.md](evals.md) | No model-quality evaluation gates |

How these notes were made: each was assembled from web research with URL and access date,
then checked against the code. They are reviewed by an independent adversarial pass (see D4 in
review-gates.md), and every `ahead` verdict is a target for that review.
