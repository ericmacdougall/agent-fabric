# ADR 0002: Every component is a first-party implementation behind a swappable provider interface

- Status: proposed (2026-11-08)
- Ticket: goodqa#15361 (program), slice F17
- Requirement source: the owner's message of 2026-11-08 asking that Fabric have its own flavor of everything and that users can swap out or add parts from any or several providers, or their own, through clear interfaces.

## Context

The parity maps ([aws-agent-parity.md](../aws-agent-parity.md)) compare Fabric with the AWS, Microsoft and Google agent stacks. All three bundle the same components (runtime, memory, tool gateway, code sandbox, browser, identity, policy, guardrails, evaluation, trace, model routing, knowledge, multi-agent and A2A). Each also lets a customer use another vendor's part for some of them. A kernel that can only run its own parts would tie a tenant to us; one that can only run vendors' parts would not work on a fresh personal install.

Two earlier constraints remain: Fabric stays model-agnostic, and nothing in our deployments, CI or tests uses Vertex AI.

## Decision

1. **First party first.** Fabric ships its own implementation of every component. A fresh install uses only these (AC-OOTB, AC-PI2).
2. **One interface per component.** The kinds are `runtime`, `memory`, `tools`, `sandbox`, `computer`, `identity`, `policy`, `guardrail`, `eval`, `trace`, `model`, `knowledge`, `agents`, `registry`, `cost` and `payments`. Each has a contract named `fabric.provider/<kind>/v1`: Go interfaces for in-process providers and a wire form (JSON over stdio, with an HTTP form later) for providers that run out of process. A contract is versioned; a breaking change is a new version that runs beside the old one.
3. **A descriptor per adapter.** Fields: `kind`, `name`, `contract`, `capabilities` (a closed list per kind), `enforcement` (`fabric_enforced`, `provider_enforced` or `advisory`), `residency`, `network` (what it must reach), `spend` (`none`, `metered` or `money`). The registry validates it and a conformance run must exercise every declared capability.
4. **Conformance is the gate.** Every kind has a suite that a provider runs against itself and that can run as a probe against a live adapter, in the same style as the harness conformance suite. A provider is bound only after it passes.
5. **Binding.** A tenant binds one provider per kind; a stream may override where the kind allows it. A run pins its bindings at start. Bindings are events, so a change is traced and needs policy permission.
6. **Fabric decides and records around every call.** The F7 policy decision and the F11 trace record are made by Fabric before and after each provider call, with the same shape whichever provider is bound. A provider that returns without Fabric's record fails the call. A capability class that policy marks must-enforce cannot be served at the `advisory` tier.
7. **Vendor adapters.** Adapters for AWS, Microsoft and Google start as an interface plus a contract test against a fake. A live adapter whose `spend` is not `none` needs a recorded owner decision before the registry enables it. A Google adapter has no live path: Fabric's deployments, CI and tests never call Vertex or `aiplatform`, hold no `aiplatform` IAM grant and run no live Vertex adapter, and a CI check fails on any such reference.
8. **Out-of-process providers** run with least privilege, with credentials from the credential provider (F4), and fail closed on crash, hang or malformed output.
9. **Portability.** Data a provider holds exports in a neutral versioned form so a tenant can leave it.

The contract names and descriptor fields are an irreversible choice (they are written into bindings, audit and policy), fixed by slice F17 and added to the ADR 0001 table as choice 8.

## Consequences

- Every first-party slice (F6, F8, F9, F10, F11, F14, F15) builds its implementation behind the contract for its kind, so F17 drafts the contracts early and in parallel with those slices.
- Policy and trace become the stable layer; providers are replaceable under them.
- The conformance suites are real work (one per kind) and must have mutants, or they would pass a provider that does nothing.
- A mixed binding (for example Bedrock models, Foundry evaluations, Fabric memory) is supported by construction and tested with fakes, not by live accounts.

## Alternatives rejected

- **Own parts only.** Simple, but a tenant cannot reuse what it already runs, and the three vendors' stacks all allow mixing.
- **Vendor parts only, with Fabric as glue.** Fails the fresh personal install and the model-agnostic rule.
- **A single plugin interface for everything.** One shape cannot state the different guarantees of a sandbox, a memory store and an evaluator; the enforcement tier and the conformance suite would be meaningless.
- **Letting providers do their own policy and trace.** A provider could skip them. Fabric records around the call instead (decision 6).
