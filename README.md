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
  --evidence`), audited in the append-only `effect_resolutions` table. A
  database guard binds every role: settled outcomes do not change except by
  retry, effects are never deleted, and a new intent or dispatch holds its
  live attempt row. A never-sent intent left by an ended attempt is settled
  with `fab effect abandon --reason --evidence`, only if it was never
  dispatched, audited in the same table. State moves follow an allow-list of the broker's
  own steps. A dispatcher holds the effect's advisory lock from before the
  DISPATCHED commit until its outcome is recorded, with Precondition plus
  transport under a deadline (60 s, or the attempt's lease end if sooner);
  at most half the pool's connections dispatch at once. A dispatch is stale
  only when its lock is free, its attempt is no longer live and it is older
  than twice that deadline; stale dispatches are listed by
  `fab effect unresolved` and read back by `fab effect reconcile`. An
  outcome whose connection died is recorded once on a fresh connection; a
  late outcome for a row that moved on is reported (`ErrSettleLost`), not
  dropped. Deploy one broker version per database with one transport
  timeout; a transaction-mode connection pooler (PgBouncer) is unsupported,
  since the lock is session-scoped.
  Limits: the app role is the reconciler, so a RECONCILING effect settles to
  whatever readback the app reports; a DISPATCHED settle needs a receipt
  naming its dispatch number and outcome, which the app role can still forge
  by hand. No daemon sweeps unresolved effects yet.
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

## Metrics

`fab metrics` prints the fabric's health once in the Prometheus text format
(0.0.4). `fab serve-metrics` serves the same thing at `GET /metrics`. It
listens on `127.0.0.1:9464` by default and refuses any address that is not
a literal loopback IP unless `--allow-remote` is passed, because there is no
auth. It has no write routes, and it runs one collection at a time; a
scrape that arrives during one gets 503. Against DNS rebinding it answers
403 to a request whose Host is a name rather than a literal IP or the listen
address, and to any request that carries an Origin header, so a scraper
must address it by IP.

Every value comes from the database, in one read-only transaction with a
statement and lock timeout (`--timeout`, default 2s). The collector takes no
row locks, so a dispatch never waits on it directly. One indirect wait
remains: while a migration's exclusive lock request is queued behind a
collection, queries behind that request wait too, for at most the timeout
plus 1s, so keep `--timeout` short. Label values are ids and enum
values only. Anything else becomes `other`, and ticket text, questions,
subjects, paths and requests are never read. Live per-process counters (a
broker's executor overruns, dispatch slots in use) arrive later, with an
in-process `serve-metrics` in the daemon.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `fabric_build_info` | gauge | version | always 1 |
| `fabric_schema_migrations_applied` | gauge | | rows in `fabric_schema` |
| `fabric_schema_level` | gauge | | number of the latest migration |
| `fabric_missions` | gauge | execution, terminal | missions by state |
| `fabric_attempts_active` | gauge | state, lease (`live`/`stale`) | LEASED, PREPARING and RUNNING attempts |
| `fabric_effects` | gauge | kind, state | effects by state |
| `fabric_effect_oldest_age_seconds` | gauge | state (QUARANTINED, RECONCILING, UNKNOWN) | oldest effect in that state, 0 when none |
| `fabric_dispatch_inflight` | gauge | | effects in DISPATCHED |
| `fabric_dispatch_overdue` | gauge | | DISPATCHED past the deadline stamped at dispatch |
| `fabric_dispatch_limit` | gauge | | configured dispatches per store |
| `fabric_sched_counter_total` | counter | name | `sched_counters` |
| `fabric_resource_capacity`, `_held`, `_spent_month` | gauge | resource, unit | dispatch-gate resources; spent is settled usage this UTC month |
| `fabric_native_spent_month_usd_micros` | gauge | | native provider cost this UTC month |
| `fabric_budget_cap_usd_micros` | gauge | scope (month, job, live_test) | fixed ceilings (constants): month USD 20, job USD 5, live test USD 1 |
| `fabric_asks_open` | gauge | kind | open asks |
| `fabric_asks_open_oldest_age_seconds` | gauge | | oldest open ask, 0 when none |

## TUI

`fab tui` is a read-only terminal view of the same state. It needs a terminal
and `FABRIC_DSN`, and it starts even while the database is down.

- **Header:** the `/metrics` gauges. These are the version, schema level, missions, attempts live and stale, effects UNKNOWN, QUARANTINED and RECONCILING, dispatch in flight against its limit and overdue, open asks, and month spend against its cap.
- **Missions pane:** active missions first, running work on top. Each row shows age and spend against the job cap.
- **Attempts pane:** LEASED, PREPARING and RUNNING attempts, each marked live or stale.
- **Effects pane:** UNKNOWN, QUARANTINED and RECONCILING effects, oldest first.
- **Asks pane:** open asks, oldest first.
- **Detail:** Enter opens the selected row's mission, with its attempts, effects, asks and decisions. Decisions are its asks of kind decision plus the effect resolutions recorded on its effects.

Keys:

| Key | Action |
|---|---|
| Up/Down, j/k | Move |
| PgUp/PgDn | Page |
| Tab, Shift-Tab | Change pane |
| Enter | Open the mission |
| Esc | Go back, or clear the filter |
| `/`, Ctrl-F | Filter |
| q, Ctrl-C | Quit |

`--keys=off` turns off the single-character shortcuts j, k, q and `/`. The arrows, Enter, Esc, Tab, Ctrl-F and Ctrl-C still work.

`--no-color`, or a set `NO_COLOR`, removes every text attribute. Colour is never the only cue:
- the focused row starts with `>`;
- a warning starts with `!` and says `disconnected` or `stale`.

`--interval` (default 2s, at least 250ms) sets the refresh rate. `--timeout` (default 1s) bounds each refresh.

Every refresh runs in one read-only transaction (`metrics.ReadOnly`), on a pool whose sessions are `default_transaction_read_only`, so the view cannot write. A row lock never delays it. Under a table lock, the refresh gives up at its timeout and the header shows `! stale` over the last data. A lost database shows `! disconnected: retrying` until it returns.

Database text is sanitized before it is drawn: ticket text, ask questions and resolution actors. Control and escape sequences, C1 controls, and bidi and zero-width characters are removed, and the text is cut to the terminal width. Effect subjects and requests, prompt bodies, ask options and errors are never shown. A failure shows only its SQLSTATE.

`fab version` prints the build revision, the same value as `fabric_build_info`.

## Providers

Every mission names its provider when it is created, and it never changes
(goodqa#15087): `fab mission new --provider claude-code|anthropic-api` is
required, and the database refuses an UPDATE of `missions.provider`.

- `claude-code`: fabd runs the mission on the Claude Code binary.
- `anthropic-api`: only `fab mission run --grant F` runs it, through the
  native loop. That loop accepts only an API-key-shaped key, sends it only to
  `https://api.anthropic.com` (no proxy), and refuses subscription billing.

Each attempt records the path that started it (`attempts.path`), and the
database refuses an attempt whose path differs from its mission's provider.
Fabd always starts `claude-code`, `fab mission run` always starts
`anthropic-api`, and `fab attempt start` requires `--path` (and
`--lock-path` for `anthropic-api`). The path is part of the start command's
request hash, so a start command recorded by an older binary and replayed
by a newer one is refused as a changed payload. Live had no in-flight
attempts at the upgrade, so nothing else was done.

Two flags are not the mission provider:

- `fab daemon --provider` is the provider binary fabd launches (`claude`,
  or a recorded fake for shadow runs).
- `fab mission run --provider anthropic` is the grant's provider field.

The native month cap is keyed on the database clock, never the host's, and
a run charges the UTC month it started in, even if it ends in the next one.

### Worker environment

A worker's environment is exactly what fabric builds, whatever fabd, the
host or the worker user's systemd manager holds. Fabric may set only these
names: `FAB_RUN`, `FAB_MISSION`, `FAB_ATTEMPT`, `FAB_FENCE`,
`FAB_RUN_DIR`, `FAB_WORKTREE`, `FABRIC_DSN`, `FABRIC_HOME`, `HOME`,
`PATH`, `LANG`, `USER`, `TMPDIR`, `SHELL`, `CLAUDE_CONFIG_DIR`,
`CLAUDE_CODE_DISABLE_CLAUDE_MDS`, `XDG_CONFIG_HOME`, `XDG_CACHE_HOME`,
`GOCACHE`, `GOMODCACHE` and `GOFLAGS` (`internal/host/env.go`). A launch
that sets any other name is refused, so a proxy or locale variable the
claude binary turns out to need is a reviewed code change. On Linux no
value goes on a command line, where sudo and systemd-run would show it to
every local account for the whole run (goodqa#15177). fabd writes the pairs
to `<run dir>/.fab-env-<run>` (mode 0640, new file, never through a
symlink), every allow-listed name is blanked with `systemd-run -E NAME=`
(so the manager's value is overridden), and `fab __sandbox --env-keep
<names> --env-file <file>` reads the file, checks that it names exactly
those names, removes it and execs the worker with only those pairs. fabd
removes the file after the run if the worker never read it. The run
directory's group must include the worker account, as the setgid
`/srv/fabric/work` tree gives it. The worker is a static fab
(`CGO_ENABLED=0`, see Deploying).

A claude-code launch always passes `--setting-sources ""`, so no user or
project settings load. The `--settings` file may hold only `permissions`,
`hooks` and `model`, and each hook only `type: command`, `command` and
`timeout` (keys are matched exactly, as Claude Code reads them). Each hook
command must be a single absolute path under `FABRIC_HOOKS_DIR`, both as
written and once symlinks are resolved. The worker user must be unable to
change the settings file, the hooks directory, each hook, or any directory
or symlink on the way to them: a file or directory owned by the worker or
writable by group or others (a directory may be writable only if it is
sticky and owned by root, like `/tmp`), or a symlink not owned by root or
fabric, refuses the launch. This matters because Claude Code reloads an
edited settings file in a running session. Managed settings apply whatever
`--setting-sources` says, so fabric also reads `managed-settings.json`,
every `managed-settings.d/*.json` and `managed-mcp.json` under
`/etc/claude-code` (Linux) or `/Library/Application Support/ClaudeCode`
(macOS). Any of these refuses the launch: an env block; a credential or
command helper (including `policyHelper`, `processWrapper` and the
`sandbox` binary paths); managed hooks, plugins or MCP servers (a managed
MCP server would inherit the worker's `FABRIC_DSN`); or a file that cannot
be read or parsed. A missing file is fine. The macOS managed-preferences
profile and server-managed settings are not read; both are controlled by
the machine's or organisation's administrators.

On Linux, workers start with `systemd-run --expand-environment=no`, so the
user manager never substitutes `$VAR` in a worker's arguments (a prompt
included). That flag needs systemd 254 or later; an older systemd rejects
it and the start fails.

### Upgrades in lockstep

A binary refuses to open or migrate a database that holds a migration it
does not embed, and names its own latest migration and the foreign one.
This is checked at start-up only: a long-running process keeps going, and
for 0026 the `attempts.path` trigger stops an older fabd. Every later
migration therefore stops every binary built before it, worker-side `fab`
calls included, until that binary is upgraded too. Upgrade all binaries
on a host together.

### Database connection

`FABRIC_DSN` must be a direct connection or a session-mode pooler, never
a transaction-pooling pgbouncer. The host reconcile lock (held by fabd and
`fab reconcile`) and each native attempt's agent-run lock are session
advisory locks. Behind transaction pooling they exclude no one: two
reconcilers could run at once, and a second native process could run the
same attempt.

## Operator actions

Some states wait for a human on purpose. Clearing them needs an explicit
operator actor, `FABRIC_ACTOR=op:<name>`, and a `--reason`. The `fab` CLI's
default actor `chat` is refused for these commands (it still works for
everything else), as are `fabd`, `native-runner`, `import` and `asks`.
This is attribution and friction, not authentication: the actor is
asserted by the caller, so any script that sets `op:<name>` passes. The
real separation is the database role; a runner role cannot write events
at all. An operator-only role for these commands is follow-up work.

- **Parked missions.** After six consecutive attempts abandoned before
  launch, a mission is parked: no attempt starts, and once nothing is live it
  is BLOCKED and frees its slot. Only an operator clears a park:
  `FABRIC_ACTOR=op:<name> fab admit --mission <id> --reason '<why>' --key <key>`. The scheduler, the native runner and the
  importer get exit 3 (goodqa#15082).
- **Paused missions.** A quarantined attempt pauses its mission, which keeps
  its slot. After checking that the quarantined runner is gone,
  `FABRIC_ACTOR=op:<name> fab unpause --mission <id> --reason '<why>' --key <key>` returns it to ADMITTED. It is
  refused while any attempt is live or unresolved, while any broker effect
  is unsettled (resolve it first), without a reason, or for a non-operator
  actor (goodqa#15085). A parked mission that is unpaused goes straight to
  BLOCKED with its park kept; `fab admit` then clears the park. The
  quarantined attempt stays QUARANTINED: nothing checks by machine that its
  runner is gone, so check the host before unpausing. An older binary refuses to write to a
  mission whose log has a `MISSION_UNPAUSED` event, so roll back with care.
- **Operator reconcile.** `fab reconcile` runs one reconcile pass for this
  host, under the host reconcile lock (exit 3 while fabd or another pass
  holds it). Its commands stay under actor `fabd`, so a pass and fabd replay
  each other. To tell the operator pass apart, it writes one stderr line,
  `fab reconcile: operator pass host=<host> actor="<FABRIC_ACTOR>" pid=<pid>
  expired=<n> quarantined=<n>`, and adds one to
  `fabric_sched_counter_total{name="reconcile_operator_passes_total"}`. A
  refused or failed pass writes no such line and does not count.
- **Final admission.** Every launch, gated or not, rechecks the attempt's
  fence, lease and intent revision against the database clock just before
  the runner starts. A database error at that moment abandons the attempt
  before launch, and it counts toward the park budget, so a database outage
  can park missions (fail closed; re-admit them afterwards).

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
- A macOS worker runs as fabd's own user, so its file, process-info and
  network access is limited by the Seatbelt profile (goodqa#15161):
  - `HOME` must be an absolute directory other than `/`, or the launch is
    refused;
  - every read under `$HOME`, under the account's directory-service home
    (an overridden `HOME` does not move it) and under `/Users` is denied.
    Reads are also denied in the directories holding other runs and other
    missions' worktrees, unless the work root sits in a shared temp
    directory. Only the run's directory, its worktree and the reviewed
    launch inputs are allowed back. The secret paths stay denied even
    inside those;
  - the ancestors of what is allowed back can still be stat'ed (metadata
    only), so git, `realpath` and Node can canonicalise paths;
  - the launch inputs are the provider's installed directory (after
    symlinks), `FABRIC_HOOKS_DIR`, the schema file and any extra absolute
    paths in `FABRIC_DARWIN_EXTRA_RO` (for example nvm's node directory
    when `claude` is an npm install). An entry is refused when it is `/`,
    a home, an account's home under `/Users`, or a directory holding a
    home, the directory of other runs, or a secret path (so not
    `~/.config` or `~/Library`). The launch runs the provider's resolved
    file, not a symlink in a denied bin directory, and the control
    `provider_starts_under_profile` runs it with `--version`. Any other
    tool on `PATH` under a home is unreadable, so install toolchains
    outside `$HOME` or list them there;
  - the worker's git uses the run's own global config
    (`GIT_CONFIG_GLOBAL`), because git exits on a denied `~/.gitconfig`;
  - reading other processes' arguments or environment (process info and
    `kern.procargs2`) is denied;
  - network is denied except outbound TCP to 443 and the database ports of
    `FABRIC_DSN` and the runner DSN (5432 when a postgres URL names no
    port), plus the DNS resolver socket. Seatbelt filters by port, not
    host, so any host on those ports is reachable; that is port-scoped,
    not mediated, egress. Unix-domain sockets are denied too, so tools
    that use a path socket for their own IPC (gpg-agent, ssh
    ControlMaster, some language servers) fail. An empty port list means
    443 on macOS but no TCP at all on Linux;
  - the profile is still allow-default for mach IPC, signals and launchd:
    a worker that asks launchd or LaunchServices to start a process gets
    an unsandboxed one, and it can signal fabd. These denies are
    goodqa#15196.
  The negative controls check a sample of this on the host before an
  untrusted provider runs:
  - `home_reads_default_denied`: canaries in each home are unreadable, and
    a run-dir file is readable in the same run;
  - `network_limited_to_allowed_ports`: an allowed loopback port is
    reached and another is refused;
  - `launch_inputs_readable`: every launch input is readable;
  - `provider_starts_under_profile`: the provider starts with `--version`.
  The unit test `TestDarwinProfileDefaultDeny` covers the rest:
  `kern.procargs2`, unix sockets, git with the worker's environment in a
  worktree under a home that holds a `.gitconfig`, and the sibling and
  allow-back boundaries.
- The grant is an in-process struct, not a signed envelope; there is no
  separate host agent yet; the worker still carries a runner database DSN
  (asks and effects).
- **Host jobs are disabled** (goodqa#15082). A run used to request a job whose
  argv the daemon launched outside the run's sandbox. Now `fab job run` and
  `fab job watch` exit 3, the runner role has no INSERT on `jobs` (and
  migration 0025 refuses one even if granted by hand), and the daemon only
  retires its own host's leftover REQUESTED rows to FAILED_TO_START. A legacy
  watcher may still record its exit. Typed, approved job templates are
  future work; until then a run cannot start work that outlives it.
- **The dispatch gate (`fab daemon --hold`) is an accident guard, not a
  security boundary yet.** It stops the scheduler from over-admitting a
  resource by mistake. It is not proven against a runner that writes the
  database directly until the runner-role restrictions (migration 0017,
  `fab db roles`) pass independent review. Runners must connect as the
  runner role (`FABRIC_RUNNER_DSN`), never as the app or owner role.
  Fabd hands a worker only that login, checked as a runner role before the
  first launch whether or not `--hold` is set; without it the worker gets
  no `FABRIC_DSN` at all, never fabd's own (goodqa#15173). The check
  refuses a runner login holding any write on any table beyond the
  runner's list (column grants, DELETE, TRUNCATE and TRIGGER included), or
  CREATE in the schema (goodqa#15167). Only the owner writes `fabric_schema`
  (0029, goodqa#15163), and every function runs with a pinned
  `search_path` that searches `pg_temp` last, so a session's temporary
  table cannot shadow a table a trigger reads (0029, goodqa#15164).
  Since 0018 a runner may only submit, dispatch, settle and retry effects
  for a running attempt, request jobs for one, and raise open asks; approval,
  effect readback and answers need the app role. These guards bind the
  runner role, not one run: the role is shared by every runner on a host, so
  any runner can act for any running mission (raise its asks, submit or
  settle its effects), and it can read every table. A runner settles
  dispatches, so a forged transport receipt for a live run is believed; a
  dispatch left by an ended run goes to the app role's readback (0019). The
  capacity gate does not depend on these rows.
- `fab metrics`, `fab serve-metrics` and `fab tui` connect with whatever
  `FABRIC_DSN` names; read-only transactions (and, for the TUI, read-only
  sessions) are the guard. A least-privilege read-only role is not yet
  provided, and `--allow-remote` metrics have no auth.
- Store tests need a disposable PostgreSQL 16 (`FABRIC_TEST_DSN`); the
  process-signalling controls are opt-in (`FABRIC_HOST_CONTROLS=1`).

## Running the tests

```
go test ./...                                   # pure packages; store tests need FABRIC_TEST_DSN
FABRIC_TEST_DSN=postgres://... go test ./...     # disposable database only
python3 -I tools/mutants/run.py --repo . --out mutants.tsv
```

Go 1.22, `pgx/v5` and `x/sys` only.

## Deploying

1. Back up the database first (`pg_dump -Fc`), check the dump with
   `pg_restore --list`, and keep the previous `fab` binary for rollback.
2. Build from a fresh `git clone` checked out at the release commit, with
   `CGO_ENABLED=0 go build -buildvcs=true -o <path> ./cmd/fab`. Check that
   `git status --porcelain` is empty, that `go version -m <path>` shows
   `build CGO_ENABLED=0` (a worker is a static fab, so it loads no shared
   library a worker-user environment could redirect), and that `<path>
   version` prints the commit's 12-character prefix; refuse to install a
   build that fails any of these, prints `devel` or ends in `-dirty`. We observed a build from a `git worktree` checkout
   (Go 1.22.2) print `devel`; the version check catches it either way.
3. Stop anything using the database, fabd included, before installing the
   new binary (a running fabd launches workers through the installed `fab`,
   and an old fabd with a new `fab` fails every start), then run
   `fab migrate` as the owner.
4. Run `fab db roles` as the owner after every migration, with the same
   password files, so the app and runner roles are granted the new tables;
   without it the app role gets `permission denied` (SQLSTATE 42501).
   `fab db roles` applies everything in one transaction, so a failed run
   changes nothing; rerun it after fixing the cause. Since 0029, fabd
   refuses to start a worker when the runner login holds any grant that
   `fab db roles` does not give, so skipping this step after an upgrade
   that changed the runner's grants stops every launch.
5. Restart, then smoke test: `fab version` and `fab metrics` (exit 0, and
   `fabric_schema_level` is the newest migration).

`fab daemon` (fabd) is deliberately not installed as a service on a Linux
live host. It stays that way until the worker sandbox passes re-review (see
Known limits). Live checks run a bounded daemon by hand against the
recorded fake worker and stop it afterwards. A mission whose attempt
was quarantined during such a check keeps its slot until an operator
checks the host and runs `fab release` or `fab unpause`.

Rollback: stop everything using the database, reinstall the previous
binary, and restore the dump, either into the same database with
`pg_restore --clean --if-exists -d <db> <dump>` or into a fresh database
that you then switch to. Everything written after the dump is lost.
The native spend ceilings are constants in the binary, so a rollback also
restores the previous binary's ceilings (before goodqa#15086 the month
ceiling was USD 100, not USD 20).

## License

Apache-2.0. See `LICENSE` for the full text and `NOTICE` for attribution.

SPDX-License-Identifier: Apache-2.0

Copyright (C) 2026 Eric MacDougall
