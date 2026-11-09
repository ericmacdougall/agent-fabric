# Release scan gate: refresh procedure and two fixes

Status: accepted approach (goodqa#15361, main decision 2026-11-09). The deploy script, the scan wrapper and the pinned manifest live in the operations directory, outside this repository; this page records the procedure and its acceptance criteria. Host paths are deliberately not named here.

## How the gate works

The live deploy runs a vulnerability scan of the candidate binary with a pinned scanner. Two conditions make the scan trustworthy: the scanner database was published at most 24 hours before the scan, and a control scan on a known-vulnerable binary, run against the same database, reports the expected findings (CONTROL OK). The control scan's output (`PAIR.ok`, which carries the database digest) is pinned in the ops manifest, and the manifest's own sha256 is a reviewed value passed to the deploy.

## Refresh procedure

The database is refreshed right before a deploy, never on a timer, so it cannot change between the control scan and the candidate scan of one deploy (a scheduled refresh could swap the cache in the middle of a deploy). The refresh is one script run by the deploy owner:

1. check the current manifest verifies;
2. move the old control scan aside (kept as evidence);
3. download the newest published database with the pinned scanner, with a cleaned environment and a fixed working directory so no scanner variable or config file can redirect it;
4. run the control scan on the recorded control binary with the recorded expected ids; the pairing file is written only on CONTROL OK;
5. write a new manifest that differs from the old one in the pairing line only (plus lines for files explicitly named for re-pinning in the same review), refusing if any other pinned file differs from its recorded hash;
6. print the new manifest sha256, which is used only after an independent review records it.

The full redesign (running the control scan at deploy time and pinning only the control binary and the expected ids, so a daily refresh needs no review) was reviewed, found workable but larger than needed, and is deferred; its findings are on the follow-up ticket.

## Fixes in this change

- The database digest covers both database files. The age check reads the metadata file while the old digest hashed only the database file, so an old database file with a fresh metadata file would have passed.
- The pairing file can no longer be overridden by an environment variable in any mode. Live mode already refused it; scratch mode honoured it.

## Acceptance criteria

- AC-SG1: the control and candidate scans of one deploy record one digest over both database files, and a change to either file changes it. Verify by: recompute the control scan's `DBFILES.txt` (two lines: the database file and the metadata file) and `DBSHA.txt` (the hash of that list) from the cache and compare with the control scan record and with `PAIR.ok`.
- AC-SG2: an environment override of the pairing file has no effect. Verify by: a scratch-mode run with the variable set to a different pairing file uses the pinned one.
- AC-SG3: the refresh changes only the pairing line and the explicitly re-pinned lines of the manifest. Verify by: the manifest diff in the refresh log.
- AC-SG4: the refresh refuses and restores the old control scan when the control scan does not report CONTROL OK. Verify by: the refresh script's own failure path, shown by running the control scan step with an id the database does not list (a one-off check; the ids are fixed in the script).
- NEG-SG1: a refresh cannot widen the expected ids or swap the control binary: the ids are fixed in the script and the script refuses unless the control binary has the recorded sha256. The script itself is not pinned by the manifest; it logs its own sha256 on every run.
- NEG-SG2: a database up to 24 hours old that keeps the expected findings passes; the gate does not detect a database that keeps those findings and drops others. That limit exists today.

## Risks

- The cache is not locked: a refresh run by hand during a deploy would still race it. The procedure forbids that; a lock is part of the deferred redesign.
- Each refresh produces a new manifest hash that needs a review, as today; `--repin FILE=SHA256` lets the same review cover other pinned files, and refuses unless each has the reviewed hash.
- The deploy script accepts any 64-hex manifest hash, so that it is a reviewed value is enforced by process only.
- The scan reads the database a moment after hashing it; a second hash after the scan is a later hardening.
- The scan wrapper's checker still describes the digest as the database file alone in a comment; correcting it needs another re-pin and is ticketed.
