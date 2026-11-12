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

Review is per call (Option A). `review_gate.safe_tools: ["edit","write"]` is
the per-mission opt-out: unreviewed writes inside the pinned worktree, `.git`
still refused, audit rows still kept.

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
- An untracked or ignored target keeps a bounded pre-image copy in the
  runner-only state directory. The git query uses the gitdir recorded at
  attempt start and refuses a repository with risky configuration; a refused or
  failed query keeps the copy.
- A call journal (pre and intended post sha256) lets a call found EXECUTING on
  resume say whether the write landed; it is never re-run.
- The result carries the path, both digests, byte counts, the copy name and a
  unified diff capped at 8 KiB.

Known limit: the review request shows the model's exact `oldText` and
`newText` (which is the change) and the optional `expect_sha256`; the computed
unified diff is in the tool result, not the hash-bound review request.

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
