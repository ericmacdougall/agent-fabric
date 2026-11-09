# Release scan gate: control scan at deploy time

Status: proposal (goodqa#15361). The deploy script and its pinned manifest live in the operations directory, outside this repository; this page records the design and its acceptance criteria. Host paths are deliberately not named here.

## Problem

The live deploy runs a vulnerability scan of the candidate binary with a pinned scanner. The scanner's database must be fresh (its own publication time at most 24 hours old), and the scan is only trusted if a control scan on a known-vulnerable binary, run against the same database, reports the expected findings (CONTROL OK). Today the control scan's output (`PAIR.ok`, which carries the database digest) is a stored file, and the pinned ops manifest pins that file. A new database therefore needs a new control scan, which changes the manifest, whose own sha256 is a reviewed value. Refreshing the database daily would need a new review every day.

## Design

1. The control scan runs at deploy time, in the same run and with the same database as the candidate scan. There is no stored pairing file to drift.
2. The manifest pins what makes the control meaningful: the control binary (by hash) and the expected finding ids (a small file). Neither changes when the database is refreshed. The scanner binary, the checker, the scan wrapper, the decisions file and the deploy script stay pinned as before.
3. The age window (24 hours), the CONTROL OK requirement and the digest equality between the control scan and the candidate scan are enforced by the pinned checker, unchanged.
4. The database refresh becomes a plain download that touches nothing pinned. It can run daily from a scheduler; it prints the database's publication time and digest for the record.

## Acceptance criteria

- AC-SG1: a deploy with a fresh database passes with no manifest change between two different database digests. Verify by: two deploys (or rehearsals) on two daily databases using the same manifest hash.
- AC-SG2: a stale database (over 24 hours) fails the deploy at the scan phase. Verify by: a rehearsal with a database aged past the window.
- AC-SG3: a database that does not report the expected control findings fails the control scan, and the candidate scan does not run. Verify by: a rehearsal with an expected id that the database does not list (covered at block level by the `unknown-id` case).
- AC-SG4: an empty or malformed expect-id file fails before any scan. Verify by: the `empty-ids` and `bad-id` cases.
- AC-SG5: changing the control binary or the expect-id file without re-pinning the manifest fails the deploy before any phase. Verify by: modify each file and run the manifest check.
- AC-SG6: the candidate scan uses the pairing written by the control scan of the same run, never a file from an earlier run. Verify by: the control directory is removed and rewritten at the start of the phase; a rehearsal that plants an older `PAIR.ok` elsewhere has no effect.
- NEG-SG1: the daily refresh cannot make a deploy pass that the control scan would fail: the refresh writes nothing the gate reads except the scanner cache, and the gate re-derives its pairing from that cache.
- NEG-SG2: the expected ids are not widened by the refresh: the ids file is pinned.

## Risks

- A deploy runs one more scan (seconds).
- Rehearsal scripts in the operations directory read a stored pairing file; they keep working only if the refresh also writes one for them. That file is no longer a gate.
- The scanner cache is not pinned. The control scan guards against an empty or hollowed database (it must list the expected findings); it does not guard against a database that keeps those findings and drops others. That limit exists today.
- The change is untested end to end until the first live deploy after install; the install keeps the previous deploy script and manifest as the rollback.

## Rollout

Install the new script and ids file, re-pin the manifest once (a reviewed hash, as today), keep the old script and manifest next to them, run one rehearsal and then the next live deploy. Add the daily database download after that.
