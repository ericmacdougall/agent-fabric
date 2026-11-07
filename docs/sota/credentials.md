# Credentials: state of the art and where Fabric stands
Status line: pinned to commit bc5a6d3; sources accessed 2026-11-07.

## What this component does in Fabric
- Native loop path: the Anthropic API key is fetched by `KeyFromCommand` inside the `fab` process (one stdout token, cached 5 minutes, error text carries command name and exit code only; `internal/provider/anthropic/keysource.go:21-57`). The key is kept in memory so it can be registered as a literal for redaction (`internal/native/native.go:186-191`). Tool children run through the host profile with an allow-listed environment and never inherit the runner's (`internal/tools/exec.go:36-41`, `HostConfiner.Run`).
- `claude -p` worker path: the launch adds `FABRIC_DSN` (the runner database role's connection string), `FAB_RUN_DIR`, `FAB_WORKTREE` and a per-run `FABRIC_HOME` to the worker environment (`internal/sched/sched.go:526`), plus `CLAUDE_CONFIG_DIR` under the run dir (`internal/provider/claude/launch.go:83-85`).
- Redaction: a 19-shape regular-expression set plus PEM block handling and exact literals (`internal/policy/redact.go:24-48,65-75`), applied to tool output at creation (`internal/agent/loop.go:491`).
- GoodQA key file for effects must exist with mode 0600 (`cmd/fab/effect.go:50-52`); the default path is under `~/.claude`, which the macOS profile denies to workers (`darwin.go:39-47`).
- Database roles: `app`, `runner` and a NOLOGIN scheduler role. The runner can SELECT every table and write only `jobs` (UPDATE), `effects` (INSERT, UPDATE), `asks` (INSERT), `effect_dispatches` (INSERT) (`internal/store/roles.go:28-35,94-102`); statements time out at 60 seconds (`roles.go:39`).

## State of the art
- Proxy-injected credentials: the secret never enters the sandbox. Claude Code on the web keeps git credentials outside the sandbox, with a proxy that checks the request and attaches the token. The local runtime has `deny` and `mask` modes: a per-session sentinel is swapped for the real value by a TLS-terminating proxy only for listed hosts. [Anthropic engineering](https://www.anthropic.com/engineering/claude-code-sandboxing) (accessed 2026-11-07), H; [Claude Code docs](https://code.claude.com/docs/en/sandboxing) (accessed 2026-11-07), H. E2B's egress proxy substitutes stored secrets outside the sandbox. [E2B](https://docs.e2b.dev/sandbox/internet-access) (accessed 2026-11-07), H.
- Setup-phase-only secrets: Codex Cloud decrypts secrets only for task setup and removes them before the agent phase; agent internet is off by default. [Codex cloud environment](https://learn.chatgpt.com/docs/environments/cloud-environment) (accessed 2026-11-07), H (legacy page).
- Short-lived scoped tokens: GitHub App installation tokens expire in one hour and can be limited to chosen repositories and permissions [GitHub docs](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-an-installation-access-token-for-a-github-app) (accessed 2026-11-07), H. Vault dynamic database credentials create a per-instance Postgres user with a lease (default TTL 1 hour, max 24) and revocation [Vault](https://developer.hashicorp.com/vault/docs/secrets/databases) (accessed 2026-11-07), H.
- Workload identity: SPIFFE SVIDs are short-lived identity documents with automatic rotation [SPIFFE](https://spiffe.io/docs/latest/spiffe-about/overview/) (accessed 2026-11-07), H. Modal can inject an OIDC identity token into a sandbox, but ordinary secrets still arrive as environment variables [Modal](https://modal.com/docs/guide/sandbox) (accessed 2026-11-07), H.
- Incidents: a 2026 research note on CI prompt injection lists CVE-2026-54316 (Claude Code, key leaked character by character through an allowed domain) and CVE-2026-12537 (Gemini CLI). Root causes: the validator saw a different string than the shell ran, the parent process environment was readable, exec privilege combined with untrusted input. Mitigations: per-run short-lived scoped credentials, separate credentials per job, egress allowlisting. [CSA research note](https://labs.cloudsecurityalliance.org/research/csa-research-note-ai-coding-agent-ci-prompt-injection-202608/) (accessed 2026-11-07), H. The earlier "Comment and Control" disclosure is known only from a secondary summary: [summary](https://vibegraveyard.ai/story/comment-and-control-ai-agents-github-secrets/) (accessed 2026-11-07), L.
- Database least privilege: Postgres row security is default-deny once enabled, with per-role policies; owners and BYPASSRLS roles bypass it unless FORCE is set. [PostgreSQL docs](https://www.postgresql.org/docs/current/ddl-rowsecurity.html) (accessed 2026-11-07), H.

## Comparison
| Practice | Fabric today (file:line and test) | Verdict | Why |
|---|---|---|---|
| Provider key kept out of the tool sandbox | Native path: key in the `fab` process, tool children get an allow-listed environment (`exec.go:36-41`, `linux.go:107`, `darwin.go:99`); `TestKeyNeverLeaks` (`internal/agent/budget_test.go:178`), `TestCaptureNeverSeesKey` (`internal/provider/anthropic/client_test.go:397`) | parity | Same result as proxy injection (key outside the sandbox) by a different route: the model call is made by the orchestrator. The tests prove the key is not persisted; they do not run a real confined child (`TestNativeToolsConfined` is skipped). |
| Provider auth for the `claude -p` worker | Not found in code I read; the worker gets `CLAUDE_CONFIG_DIR` in the run dir (`launch.go:83-85`) | not verified | How the CLI obtains its credential on this path is outside the read files. |
| Key retrieval hygiene | Single-token check, no output in errors, TTL cache; `TestKeyFromCommand` (`client_test.go:363`), `TestKeyNotInErrors` (`client_test.go:346`) | parity | Comparable to a secret-manager call; no per-run lease or revocation. |
| Per-run short-lived credential | None. One static runner DSN for all workers (`sched.go:168,526`) | behind | Vault and GitHub Apps mint per-run, expiring credentials. SQL actions cannot be attributed to one worker by role name. |
| Database least privilege | Runner role: SELECT everything, write four tables; `TestRolesLeastPrivilege` (`roles_test.go:51`), `TestRunnerCannotApproveEffects` (`roles_test.go:173`), `TestRunnerCannotShadowGuardTables` (`roles_test.go:256`). `NOBYPASSRLS` is set (`roles.go:83`); no row-level security policy found | parity on write scope; behind on read scope | Writes are narrow and tested. A worker holding the DSN can read other missions' rows; the TCP port limit still allows the database port (`os_linux.go:22-27`). |
| Proxy-injected provider or git credentials | None; no egress proxy (see sandboxing) | behind | Claude Code (`mask`) and E2B inject at the network layer; a worker-held secret can be sent to any host on 443. |
| Environment allow-list | Both Start paths build an explicit environment; macOS control `environment_is_allow_listed` (`darwin.go:356`); no Linux control found | parity | Matches the fix for the parent-environment root cause in the 2026 incidents, with a test on macOS only. |
| Output redaction | 19 shapes plus PEM plus literals; `TestRedactorShapes` (`policy_test.go:30`), `TestToolOutputRedactedAtCreation` (`loop_test.go:140`) | parity | Shape matching detects, it does not prevent exfiltration. The code comment says recall is favored over precision (`redact.go:20-23`). |
| Secret files unreadable to workers | macOS deny list (`darwin.go:39-47`); Linux read allow-list; controls `secret_paths_unreadable` | parity (Linux), behind (macOS) | Deny lists miss unnamed paths. |
| Credential file mode | 0600 check, mode only, not owner (`effect.go:50-52`); no test found | behind | A file owned by another user with mode 0600 passes. |

## Upgrades, ranked
| rank | upgrade | value | effort | gate or risk addressed | ticket: TBD |
|---|---|---|---|---|---|
| 1 | Per-run Postgres role (or `SET ROLE` session) with `VALID UNTIL`, dropped when the worker ends; usernames then attribute SQL to a run | high | med | static long-lived DSN in the worker | TBD |
| 2 | Add negative tests: a confined child cannot read the parent's environment or `/proc/<pid>/environ`; a Linux environment control | high | low | parent-environment root cause | TBD |
| 3 | Row-level security with FORCE on run-scoped tables, keyed to the worker role | med-high | med | cross-mission reads through the DSN | TBD |
| 4 | Credential-injecting egress proxy; sentinel values in worker environment | high | high | exfiltration of worker-held secrets (pairs with sandboxing rank 1) | TBD |
| 5 | Document "worker may hold X for N minutes" per credential class | med | low | threat model clarity | TBD |
| 6 | Owner and mode check for key files, with a test | low | low | misplaced key file | TBD |
| 7 | Workload identity (SPIFFE/OIDC) for workers calling external services | med | high | static secrets | TBD |

## Not verified
- How the `claude -p` worker authenticates to its provider, and whether any provider key is present in that worker's environment.
- Whether a same-UID worker can read the daemon's `/proc/<pid>/environ`: `/proc` is in the Landlock read list (`landlock_linux.go:98`), so this is an inference from the code, not a tested result. The dedicated-account design would close it.
- Whether `FABRIC_DSN` carries a password in the DSN string (role passwords are never logged, `roles.go:44-46`) and how it is provisioned.
- The incident details of "Comment and Control" (search-result text only).
