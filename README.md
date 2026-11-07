# agent-fabric

A small control plane for running bounded, disposable coding-agent workers:
missions, attempts with fences, an effect broker with idempotent readback,
host-owned jobs, and event-driven relaunch instead of polling. One daemon
over Postgres is the only writer; workers are `claude -p` (or a recorded
fake) processes launched by a host profile that confines them.

**Status: early, shadow-only, under review.** Nothing here drives a live
system yet. The code is being built against a design document and an
independent code inspection; findings are fixed in commits that cite the
inspection. Treat every claim below as "tested in the ways stated", not as
production readiness.

## What exists

- `internal/domain`, `internal/reducer`: the pure state machine (three
  independent version counters, four state dimensions, a separate invariant
  oracle, seeded property tests, must-fail mutants in `tools/mutants`).
- `internal/store`: the Postgres authority. One transaction per command with
  stable replay under an idempotency key, gap-free per-mission events, a
  transactional outbox that cannot skip late commits, attempts with
  database-time leases.
- `internal/effects`: the effect broker (`INTENT -> DISPATCHED -> CONFIRMED |
  FAILED_DEFINITE | UNKNOWN -> reconciled by readback`), approval bound to the
  request hash, a fake executor and a GoodQA ticket-comment executor. A
  readback conflict leaves an effect `QUARANTINED`; only an app-role operator
  can settle it (`fab effect resolve --outcome applied|not_applied --reason
  --evidence`), audited in the append-only `effect_resolutions` table.
- Done means live-tested: `fab done` is refused until `fab live-test` records a
  receipt for the deployed commit of the current candidate, and while any of
  the mission's effects (reducer or broker) is unsettled.
- `internal/host`: Linux (systemd user unit + cgroup inventory + Landlock) and
  macOS (own session + Seatbelt + inventory by token and inherited descriptor)
  profiles, each with a negative-control suite that must pass before the
  profile advertises a boundary.
- `internal/provider/claude`: a bounded stream-json parser and a launch line
  built only from a policy grant (no model names in this repository).
- `internal/sched`, `cmd/fab`: the daemon loop, jobs, waits, a CLI, and a
  recorded fake provider for shadow runs.

## Known limits (honest list)

- **Linux sandbox findings are still being fixed.** A code inspection found
  that on a same-UID host a worker can reach the user manager's bus and start
  a sibling unit outside its cgroup and Landlock domain, and can signal
  same-UID processes. The repair runs workers under a dedicated locked
  account and verifies stops by cgroup emptiness; until re-review passes, no
  real provider runs on Linux, only the recorded fake.
- macOS cannot see the environment of Apple platform binaries; attribution
  uses an inherited descriptor as a second signal, and a platform binary that
  calls `setsid` and closes inherited descriptors is still invisible.
- The grant is an in-process struct, not a signed envelope; there is no
  separate host agent yet; the worker still carries a database DSN for
  `fab job run`.
- **The dispatch gate (`fab daemon --hold`) is an accident guard, not a
  security boundary yet.** It stops the scheduler from over-admitting a
  resource by mistake. It is not proven against a runner that writes the
  database directly until the runner-role restrictions (migration 0017,
  `fab db roles`) pass independent review. Runners must connect as the
  runner role (`FABRIC_RUNNER_DSN`), never as the app or owner role.
  Since 0018 a runner may only submit, dispatch, settle and retry effects
  for a running attempt, request jobs for one, and raise open asks; approval,
  effect readback and answers need the app role. These guards bind the
  runner role, not one run: the role is shared by every runner on a host, so
  any runner can act for any running mission (raise its asks, submit or
  settle its effects), and it can read every table. A runner settles
  dispatches, so a forged transport receipt for a live run is believed; a
  dispatch left by an ended run goes to the app role's readback (0019). The
  capacity gate does not depend on these rows.
- Store tests need a disposable PostgreSQL 16 (`FABRIC_TEST_DSN`); the
  process-signalling controls are opt-in (`FABRIC_HOST_CONTROLS=1`).

## Running the tests

```
go test ./...                                   # pure packages; store tests need FABRIC_TEST_DSN
FABRIC_TEST_DSN=postgres://... go test ./...     # disposable database only
python3 -I tools/mutants/run.py --repo . --out mutants.tsv
```

Go 1.22, `pgx/v5` and `x/sys` only.

## License

Apache-2.0. See `LICENSE` for the full text and `NOTICE` for attribution.

SPDX-License-Identifier: Apache-2.0

Copyright (C) 2026 Eric MacDougall
