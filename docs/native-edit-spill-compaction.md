# Native loop: edit and write, output spill, compaction

GoodQA goodqa#15361 (pi-dev port N1 to N3). Designs only are ported from
earendil-works/pi (MIT; see NOTICE). Decision on review policy: main (an
agent), on goodqa#15361, Option A.

## N1: edit and write

`edit` and `write` change files inside the pinned worktree. They exist only
when both hold:

- the operator flag `FABRIC_REVIEW_GATE=1` is on, and
- the grant sets `review_gate.mode` explicitly to `evaluate` or `enforce`.

With the flag off, with no explicit mode, or in `shadow` mode, the tools are
absent from the tool list, so a model that asks for one gets the loop's
unknown-tool result. In `evaluate` mode the gate records the call and never
executes it. Only `enforce` executes.

Review is per call (Option A, with the red-team's conditions C1 to C5 built
in; goodqa#15864 to #15867). Default: off. The tools exist only under the two
conditions above, and nothing here changes that.

- What the approver sees (C1). Each edit or write request carries a binding
  that the runner computes from the worktree, not from the model: the target
  path, the target file's current sha256 (`new` for a file that does not
  exist), the sha256 of the bytes the call would leave, the sha256 of the whole
  diff, and the capped text of the change (the diff, and for a write the new
  content as well; at most 16 KiB of each, cut with a `[TRUNCATED ...]` marker
  that names the digest of the whole). The binding is part of the request hash.
  `fab review show HASH` prints it; control and bidirectional-format characters
  in the path or the text are shown as `\u{XXXX}` escapes. `fab review list`
  shows the path and digests, not the change. Approving a change that was cut
  at the cap needs `fab review approve --accept-truncated`. Prompts, model
  output and the call's other arguments are still never shown.
- An approval holds for the file state it was given for. At execution the
  runner recomputes the binding from the file as it is and refuses unless it is
  identical (path, sha256, diff); it then hands the tool the reviewed sha256 as
  `expect_sha256`, so the tool re-checks under its own lock. A file that changed
  after the approval makes a new request that needs its own approval. The
  refusal is definite (`FAILED_DEFINITE`, nothing written).
- Requests from before bindings (the older hash form) are shown as `unbound`,
  can be rejected, and never execute: the policy requires a binding
  (`RequireBinding`, part of the policy digest), the executor refuses a request
  without one, and the call hashes differently on the new path.
- `review_gate.safe_tools` is the per-mission opt-out. It names `edit` and
  `write` only (C2): `shell` or anything else is refused at validation, and the
  policy labels `shell` as unknown-undo, so every shell call needs a human. A
  grant with a non-empty `safe_tools` does not start until an operator has
  recorded an approval (C3): `FABRIC_ACTOR=op:<name> fab review
  approve-safe-tools --grant <file> --reason <why>` appends a
  `SAFE_TOOLS_APPROVED` row to the audit chain with the operator, the time, the
  mission id and the digest of the exact policy the grant produces (there is no
  new table). A row for another mission or another policy digest, by a
  non-`op:` actor, or of another event does not count. No tool the model holds
  can write it, and the database refuses audit writes from the agent and runner
  roles. Unreviewed writes still stay inside the pinned worktree with `.git`
  refused, and audit rows are still kept.

Safety properties:

- Every path step is `openat` with `O_NOFOLLOW` from a root descriptor checked
  by `WorktreePin.CheckFD`. `..`, absolute paths outside the root, symlinks,
  hard links and non-regular files are refused.
- Any component that folds to `.git` (case, HFS ignorable code points, NTFS
  trailing dot or space, `git~1`) is refused.
- `edit` matches each `oldText` in the original bytes (exact first, then an
  NFKC, quote, dash and space fold), requires exactly one match, refuses
  overlaps, keeps CRLF and a BOM, and applies spans mapped back to the
  original offsets. At most 64 edits; file and result at most 1 MiB.
- Commit is a per-(directory, name) lock, a sha256 pre-image check, a temp file
  `.fabric-<callid8>.tmp`, a second check right before `renameat`, then fsync.
  A changed file is refused with "file changed since read".
- Reversibility (why the policy labels edit and write `Reversible`): before
  an existing file is replaced, the runner keeps a copy of the exact bytes in
  `<state_root>/preimage/<attempt>/` (0700, own uid, never under RunDir),
  unless git already holds those bytes in a gitdir the child cannot write
  (goodqa#15865). "Holds" means the bytes' blob id
  (`git hash-object --no-filters`) equals both the stage-0 index entry and
  `HEAD:<path>`. Then the result names `preimage_git_blob: <id>` and no copy is
  made. The shortcut is taken only when the recorded gitdir, after symlinks,
  lies outside every tree the confined child can write: the worktree, the run
  directory, the system temp roots (`/tmp`, `/private/tmp`, `/var/tmp`,
  `/private/var/folders`, `$TMPDIR`, `os.TempDir()`), `/dev` and the host
  profile's extra writable roots. An ordinary checkout keeps its gitdir at
  `<worktree>/.git`, inside the worktree, so it always gets a copy: a child that
  can write that gitdir can commit the bytes and then erase the blob. A
  separate or linked gitdir outside those roots keeps the shortcut. An unknown
  or unresolvable root list never takes it. Untracked, ignored, dirty,
  staged-only and conflicted files, a repository with no HEAD commit, a clean
  filter that changes the bytes, risky repository configuration, a replaced
  gitdir, and any git failure or timeout all keep a copy
  (`preimage_copy: pre-<id>`). Edit and write are never built without this
  store.
- The store must be out of the child's reach (goodqa#15865). Edit and write are
  not built, and the run does not start with them, when the state directory
  (`FABRIC_STATE_DIR`, default `~/.local/state/fabric`) resolves, after
  symlinks, under the worktree, the run directory, a system temp root or any
  child-writable root; the error says which. The default is accepted. On the
  unconfined fake host the child can write everywhere, so edit and write do not
  start there. Tests whose state directory is a `t.TempDir()` use
  `AllowStateDirUnderForTest` (export_test.go, compiled into test binaries
  only), which exempts that one path from the temp-root clause and nothing
  else; no environment variable or flag turns the check off.
- Copies are never evicted. Each attempt holds at most 256 copies or 256 MiB.
  Beyond that, edit and write are refused with "pre-image store is full ...;
  the call was refused so that no earlier copy is removed", and the target is
  unchanged. Only `pre-*` files count; call journals (`j-*`) and the gitdir
  record do not.
- The gitdir is recorded once per attempt, in the attempt's pre-image
  directory (`gitref`: path, device and inode, or "none" when the worktree had
  no git; created `O_EXCL` 0600 by the runner). A resumed runner uses that
  record and never reads the worktree's `.git` again, which the child can
  rewrite. If the recorded gitdir was replaced, the runner refuses it and
  copies every existing target. A filesystem whose device numbers change
  across a reboot fails closed the same way.
- A call journal (pre and intended post sha256) lets a call found EXECUTING on
  resume say whether the write landed; it is never re-run.
- The result carries the path, both digests, byte counts, the copy name and a
  unified diff capped at 8 KiB.

Pre-image retention (operator step; nothing removes copies automatically).
A copy is the only way to undo an overwrite of bytes git did not hold, and
it may be needed long after its attempt ends, until a human has reviewed the
change it made. So the runner never deletes `preimage/<attempt>/`; only the
spill directory is reaped. An operator may remove an attempt's pre-image
directory only when all of these hold:
1. The attempt is terminal. It does not appear in `SELECT id FROM attempts
   WHERE state IN ('LEASED','PREPARING','RUNNING')`, and no runner holds its
   lock.
2. The attempt's changes were reviewed and accepted, or deliberately thrown
   away, by a human.
3. The retention period has passed: 14 days after the attempt ended, by
   default.
Then remove that one directory by its literal path (for example
`rm -r <state_root>/preimage/<attempt-id>`), never with a wildcard over
`preimage/`. Disk use: `du -sh <state_root>/preimage/*`. Each attempt is
capped at 256 MiB.

Limits of the approval view: a change longer than the display cap is shown in
part (the whole is bound by its sha256, and approving it needs
`--accept-truncated`); the view cannot tell an operator that a change is wise;
and a gitdir that the child cannot write but that shares objects through
`objects/info/alternates` with a tree it can is not examined.

## N2: output spill

When a shell or grep result is cut, the runner copies the full output from the
descriptor it already holds into `<state_root>/spill/<attempt>/` (0700, own uid,
held open with `O_NOFOLLOW`; ids are 16 hex characters from `crypto/rand`,
created `O_EXCL`). The notice names `spill:<id>`. `read` accepts `offset` and
`limit` (1-based lines, at most 2000) and `spill:<16 hex>`, which resolves only
in the current attempt's directory. Spill reads pass through the loop's
Redactor. At most 16 files and 64 MiB per attempt, oldest evicted first
("spill expired"). The directory is removed when the attempt finishes; orphan
directories of attempts that are not running are reaped at runner start.

## N3: compaction

Off by default. A grant opts in with `compaction`:

| field | server | client |
|---|---|---|
| `mode` | `"server"` | `"client"` |
| `trigger_tokens` | 0 or at least 50000 (the API trigger) | 0, or 8000 to `window_tokens` (estimated input, bytes / 3.5) |
| `compaction_output_cap` | required, 1 to 32768 (used in the hold) | optional, 256 to 16384 (summary max_tokens, default 4096) |
| `window_tokens` | not used | required, 8000 to 1000000 |
| `keep_prefix` / `strip_thinking` | `keep_prefix` | `strip_thinking` (default off; see below) |

`extra.context_management` and any beta outside the allowlist
(`compact-2026-01-12`) are refused at validation.

Mode S sends `context_management.edits = [{type: compact_20260112,
instructions, trigger?}]` and the beta header. The instructions forbid tool
calls: the docs warn that with tools defined the summarising step can call a
tool and return a null summary. The compaction block is stored verbatim and
messages before one that holds a summary are omitted from the next request.
The hold is `WorstCase(body, max_tokens) + WorstCase(body, output cap)` and
cost sums `usage.iterations` (the API excludes compaction passes from the top
level). A settled cost above the hold parks the attempt as budget-exhausted
and settles the full actual.

Mode C fires on a `model_context_window_exceeded` stop, a "prompt is too long"
400 or the estimated-size trigger. It cuts at the oldest assistant message
whose suffix fits half the window, sends one summary request under its own
hold (a refused hold parks, nothing is sent) and commits an append-only
`agent_compactions` row. A resume reads the row and never regenerates it. One
compact-and-retry per overflow; a second overflow parks. An empty or null
summary parks safely with no row and no cut.

Preserved thinking under mode C is decided by the live N3-THINKING check; if
the API rejects kept thinking blocks, set `strip_thinking`.

Docs source: https://platform.claude.com/docs/en/build-with-claude/compaction-threshold
(read 2026-11-12).

## Rollback

Unset `FABRIC_REVIEW_GATE` or the grant's mode to remove edit and write; remove
`compaction` from the grant; spill is additive. Migration 0039 is roll-forward
only.
