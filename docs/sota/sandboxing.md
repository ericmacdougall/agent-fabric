# Sandboxing: state of the art and where Fabric stands
Status line: pinned to commit bc5a6d3; sources accessed 2026-11-07.

## What this component does in Fabric
- Every worker runs under a per-run `Grant` (`internal/host/grant.go`, `Validate` fails closed) and a host profile. macOS: `sandbox-exec` with a generated Seatbelt profile, `(allow default)` then `(deny file-write*)` except the run dir, `/private/tmp`, `/private/var/folders`, `/dev` and, for profile P1 (writer), the worktree; a fixed deny-read list covers `~/.ssh`, `~/.claude`, `~/.aws`, keychains and similar (`internal/host/darwin.go:39-77`).
- The macOS profile refuses P1 (writer) grants unless the test runner sets `AllowP1` (`darwin.go:81`; test `TestDarwinRefusesWriterGrants`, `internal/host/host_test.go:88`). The Mac runs review (P0) and browser lanes only.
- Linux: a `systemd-run --user` transient unit per run (one cgroup, `KillMode=control-group`), optional `MemoryMax`/`CPUQuota`/`TasksMax`, then `fab __sandbox` applies Landlock (filesystem read allow-list, write only to run dir, tmp and, for P1, the worktree) and `no_new_privs` (`linux.go:92-135`, `landlock_linux.go:103-177`). With Landlock ABI 4 or newer the unit may connect only to TCP ports 443 and the database port (`cmd/fab/os_linux.go:21-30`).
- An optional dedicated account (`FABRIC_WORKER_USER`, run through scoped sudo) removes the same-UID escape paths (`linux.go:43-80`). The daemon refuses an untrusted provider unless every negative control passed (`cmd/fab/job.go:100-114`, `host.Capabilities` in `internal/host/control.go:49-59`); the native loop does the same with required control names (`internal/tools/exec.go` `RequireBoundary`; test `TestRefuseUnadvertisedHost`, `internal/tools/tools_test.go:243`).

## State of the art
- Both a filesystem and a network boundary are needed; either alone is bypassable. Claude Code uses a cwd-scoped filesystem plus a proxy over a Unix socket, on Seatbelt and bubblewrap. [Anthropic engineering](https://www.anthropic.com/engineering/claude-code-sandboxing) (accessed 2026-11-07), H.
- Per-command network namespace with a domain-allowlist proxy; allow lists start empty, the cloud metadata address is refused, UDP/HTTP3/ICMP cannot leave. Claude Code documents its own weak settings (nested user namespaces, `excludedCommands`, `allowAllUnixSockets`); native Windows is unsandboxed (WSL2 only). [Claude Code docs](https://code.claude.com/docs/en/sandboxing) (accessed 2026-11-07), H.
- Codex: Seatbelt on macOS, bubblewrap on Linux, a native sandbox on Windows; modes read-only, workspace-write, full-access; the sandbox is separate from approval policy. Codex Cloud has agent-phase internet off by default. [Codex sandboxing](https://learn.chatgpt.com/docs/sandboxing) (accessed 2026-11-07), H; [cloud environment](https://learn.chatgpt.com/docs/environments/cloud-environment) (accessed 2026-11-07), H (marked legacy).
- Landlock is unprivileged, stackable and irreversible; filesystem since 5.13, TCP bind/connect ABI 4, UDP ABI 10, abstract-socket and signal scoping ABI 6. It cannot restrict some operations (chdir, stat, chmod) or sockets reached through `/proc/<pid>/fd`. [kernel docs](https://docs.kernel.org/userspace-api/landlock.html) (accessed 2026-11-07), H.
- Same-UID escape precedent: CVE-2026-47128 (nono), a Landlock/seccomp policy that allowed Unix sockets reached the per-user systemd bus and `systemd-run --user`; fixed by blocking sockets. [advisory](https://fieldguide.lutrasecurity.com/CVE-2026-47128/) (accessed 2026-11-07), H.
- `DynamicUser=` gives a transient UID and implies `NoNewPrivileges`, `ProtectSystem=strict`, `ProtectHome=read-only`. [systemd.exec](https://www.freedesktop.org/software/systemd/man/latest/systemd.exec.html) (accessed 2026-11-07), H (first 100k characters only).
- Stronger isolation tiers: gVisor (userspace kernel, relies on host cgroups for denial of service, "a sandbox is not a substitute for a secure architecture") [gVisor](https://gvisor.dev/docs/architecture_guide/security/) (accessed 2026-11-07), H; Firecracker microVMs need KVM [Fly.io comparison](https://fly.io/learn/firecracker-vs-gvisor/) (accessed 2026-11-07), M (vendor-written).
- Hosted agent sandboxes: Modal (gVisor or VM runtime) [docs](https://modal.com/docs/guide/sandbox) (accessed 2026-11-07), H; E2B (IP/CIDR/domain allow-out, matched by HTTP Host or SNI, no QUIC) [docs](https://docs.e2b.dev/sandbox/internet-access) (accessed 2026-11-07), H; Cloudflare sandbox containers in a microVM with egress off by default [docs](https://developers.cloudflare.com/sandbox/) (accessed 2026-11-07), M.
- Windows primitives: AppContainer (default-deny files, registry, network) [Microsoft](https://learn.microsoft.com/en-us/windows/win32/secauthz/appcontainer-isolation) (accessed 2026-11-07), H; Job Objects (group kill, limits; process management, not isolation) [Microsoft](https://learn.microsoft.com/en-us/windows/win32/procthread/job-objects) (accessed 2026-11-07), H.
- `sandbox-exec` is deprecated on paper but still used, with no Apple-supported CLI replacement and no DNS-aware egress control; Virtualization.framework is the supported VM route. Sources were search snippets only: [HN thread](https://hn.svelte.dev/item/44283454) (accessed 2026-11-07), L.

## Comparison
| Practice | Fabric today (file:line and test) | Verdict | Why |
|---|---|---|---|
| Filesystem write limits | macOS Seatbelt write deny (`darwin.go:61`), Linux Landlock (`landlock_linux.go:103`); controls `seatbelt_denies_write_outside` (`darwin.go:334`), `landlock_denies_write_outside_worktree` (`linux.go:514`) | parity | Same primitive family as Claude Code and Codex. Fabric adds a read-only worktree for P0 review runs (`linux.go:118`). |
| Filesystem read limits | Linux: read allow-list (`landlock_linux.go:98`). macOS: `(allow default)` plus a nine-path deny list (`darwin.go:39-47`); control `secret_paths_unreadable` (`darwin.go:340`, `linux.go:534`) | parity (Linux), behind (macOS) | A deny list misses any secret path not named, such as other dotfile credential stores. |
| Network egress | Linux: TCP connect limited to port 443 and the DB port, ABI 4 only; control `tcp_connect_limited_to_allowed_ports` (`linux.go:558`). macOS: none. No proxy, no domain list, UDP unrestricted | behind | Claude Code, E2B and Codex Cloud filter by domain or default to off. Port 443 to any host still permits exfiltration of anything readable. |
| Kernel without Landlock ABI 4 | Net rules are added only `if abi >= 4` and no error otherwise (`landlock_linux.go:117,160`); no test found | behind | An older kernel launches without the port limit and without a warning. |
| Same-UID control-plane escape | Same-UID: two controls fail by design, `controlling_user_manager_unreachable` (`linux.go:567`) and `controlling_user_process_unsignallable` (`linux.go:582`). Dedicated account is implemented (`linux.go:43-80`) but no passing run is recorded here | behind (same-UID); parity (peer not documented) for the dedicated-account design | nono's CVE shows the same hole. Fabric reports it, but a same-UID host never reaches containment true. |
| Launch gated on demonstrated containment | `job.go:100-114`; harness prints `containment demonstrated: false` on a same-UID host | parity (peer not documented) | The cited peers publish weak settings but do not document a self-test that blocks launch. A peer's silence is not proof it lacks one. |
| Process-tree kill | cgroup stop with inventory verification and `Verified` receipt; `cgroup_stop_reaps_setsid_grandchild` (`linux.go:502`), `TestRV12_StopFailsClosed` (`review_linux_test.go:18`) | parity | Job Objects and cgroups give group kill; Fabric adds a verified-empty check. |
| Resource limits | `MemoryMax`/`CPUQuota`/`TasksMax` fields (`linux.go:102`); production wiring sets none (`os_linux.go:30`); only the `memorymax_ends_allocation` control (`linux.go:524`) sets 64M | behind | The capability exists; the default is the unit default. |
| Environment scrubbing | Explicit allow-list in `Linux.Start` (`linux.go:107`) and `Darwin.Start` (`darwin.go:99`); macOS control `environment_is_allow_listed` (`darwin.go:356`); no Linux control found | parity (macOS), not verified (Linux) | Both pass PATH, HOME and a few fixed variables plus the caller's list. |
| VM-grade tier | none | behind | Peers offer gVisor, Firecracker or a microVM; Fabric shares the host kernel. |
| Windows | none | behind | Claude Code and Codex cover WSL2 or a native sandbox. |
| Documented weak settings | this document only | behind | Claude Code publishes a list; Fabric has none yet. |

## Upgrades, ranked
| rank | upgrade | value | effort | gate or risk addressed | ticket |
|---|---|---|---|---|---|
| 1 | Egress proxy with default-deny domain allowlist and metadata-address block; macOS Seatbelt network rule pointing at a loopback proxy | high | high | exfiltration of any readable secret | goodqa#15137 |
| 2 | Record a dedicated-account run with all 10 controls passing, and make it the gate for writer lanes | high | low | containment false today | TBD |
| 3 | Fail or warn when Landlock ABI is below 4; add a negative control; probe ABI 6 scope flags | med | low | silent loss of the port limit | TBD |
| 4 | Replace the macOS deny list with a read allow-list | med | med | unnamed secret paths | TBD |
| 5 | Set default `MemoryMax`/`CPUQuota`/`TasksMax` in production wiring; add a Linux environment control | med | low | resource and env-leak regressions | TBD |
| 6 | Optional microVM profile for untrusted repositories | high | high | shared host kernel | TBD |
| 7 | Windows: documented WSL2-only support, or Job Object plus AppContainer | med | med-high | no Windows backend | TBD |
| 8 | Publish a weaker-settings table | low | low | operator awareness | TBD |

## Not verified
- The control results (8 pass, 2 fail by design, containment false) come from an earlier measurement on a same-UID Linux host. I did not re-run `TestNegativeControls`; it is skipped unless `FABRIC_HOST_CONTROLS=1` (`host_test.go:43`).
- The dedicated-account path is implemented, but I found no recorded run where the two same-UID controls pass.
- Live confinement of native tools is not tested: `TestNativeToolsConfined` (`internal/tools/tools_test.go:259`) is skipped as BLOCKED.
- The Linux environment allow-list and the ABI-below-4 behavior have no test that I found.
- Apple's own documentation, Windows restricted tokens and systemd seccomp/IP-deny directives were not read.
