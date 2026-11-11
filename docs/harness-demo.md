# Bounded native-provider demo

`fab harness demo --provider claude|codex --workspace DIR --events FILE`
drives one new owned native subscription session and verifies `demo.txt` is
exactly `fabric` followed by one newline. The optional `--mc-events FILE`
projects only fixed session lifecycle metadata for explicit MC FileDrop.
These are external native-provider jobs, separate from daemon dispatch.

This experimental command pins the reviewed Darwin client versions and local
mandatory policy/hook identities. It is not a portable provider setup command.
Build and tests run on the fleet; the Darwin binary is a demo artifact, not a
Linux service release. No API keys or credential copies are accepted.

Before any native startup, independent code review and main sign-off must cover
the exact source, controls and run kit. Public synthetic probes must demonstrate
the generated Seatbelt policy, including mediated TLS success, direct and
downgrade denial, log unlink/rename denial and a deliberately widened control.
Claude instruction/rule/import/memory controls also need a pinned-native public
fixture observation and broken control. Passing Go fakes is not that evidence.

Claude uses native restricted mode, Write only and manual host permission
callbacks. A one-use callback binds the exact owned path/content and grants no
lasting permission. Automatic context discovery is disabled; a hash-bound
snapshot appends mandatory current policy while preserving the native prompt
and all required hooks. Optional detached pruning and native auto-updates are
disabled only in the child environment. Native managed-policy doctor output is
reduced privately under a finite deadline; unknown/cached/failed status refuses.

Both clients inherit the outer network and log-preservation profile. The only
outbound worker TCP destination is its own numeric loopback mediator. The
mediator permits compiled provider destinations and matching TLS SNI, pins DNS,
and relays opaque TLS. There is no TLS interception or full filesystem/process
containment claim. Required trusted hooks may have separate sessions; on
cancellation, retained job state and uncertain hook cleanup are reported as
failure. Do not infer that all descendants are gone from a leader exit.

Each job has a 120-second controller deadline and bounded output. An unknown
tool/catalog/input, missing callback, quota/auth error, unsupported model or
unexpected artifact ends that attempt. There is no automatic provider retry,
new login or fallback. Existing logs and owned failure evidence are retained.

Hook completion is part of success: starts and responses bind the same private
ID/event/name, every required selected hook must return exit zero and success,
and no hook may remain outstanding. The pinned snapshot selects three startup,
four prompt, two Write pre-tool and five Stop executions; all 32 registrations
remain installed. Hook output is never copied to the public journal.

Codex uses one fresh task-private directory for both `sqlite_home` and `log_dir`,
so the pinned client's legacy log path is absent. Existing user log SQLite files
and their journal siblings, and the old log directory, deny all writes in the
outer profile. Preventing unlink alone would not prevent SQL row retention from
deleting old records. The proxy allowance uses TCP specifically; same-port UDP
denial needs its own public fixture control.
