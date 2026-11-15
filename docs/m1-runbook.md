# M1 operator runbook: holds and releases

This runbook covers goodqa#15966, spec v16, AC-34 and AC-35(i).

An M1 fabd puts an **operator hold** on a mission in these cases:

- a settle failed closed;
- the subscription token was refused (AC-19);
- an attempt's binding record was lost (section 5).

While the hold stands, the mission starts nothing. Only an operator can lift it. This page explains how to inspect a hold and release it safely.

## Where things are

| What | Path |
|---|---|
| Run area | `{run area}`: the `--run-area` flag, default `{work-root}/run-area`. It is fabd-only, mode 0700. |
| Records | `{run area}/records/{attempt}.json`, holding the AC-29 records |
| Binding records and settled markers | `{run area}/bindings/{attempt}.json`, `{attempt}.settled`, `{attempt}.material` |
| Raw and redacted streams | `{run area}/streams/{attempt}/` |
| Worker run roots | `{work-root}/run/{attempt}/` (`claude-config`, `home`, `tmp`) |

The hold itself is the row `sched.ophold.{mission}` in `outbox_cursors`. Its `last_id` is the hold's random generation. The row carries no reason.

## 1. Find the hold and its class

Run these as the fabd user:

```
fab mission show M
jq '.[] | select(.kind=="mission_hold")' {run area}/records/*.json
```

The `mission_hold` record names the held attempt and the class. The classes are:

| Class | Meaning | Sweep ran? |
|---|---|---|
| `credential_material_unavailable` | The bound values could not be loaded. | No |
| `credential_rotated_mid_attempt` | The GoodQA key or a DSN changed during the run. | No |
| `redaction_numeric_hit` | A credential sat inside a JSON number. | Yes |
| `redaction_broke_stream` | Redaction would break the stream. | Yes |
| `credential_rejected` | The token was refused (AC-19). Rotate it first. | Yes |

## 2. Classes that never swept: remove the run roots first

This applies to `credential_material_unavailable` and `credential_rotated_mid_attempt`. It also applies when there is no `mission_hold` record, which the release treats the same way.

Nothing read or swept the worker's files, and no sweep material was kept. So these roots cannot be re-swept, and they may still hold the token.

Before you release, remove the attempt's run roots **as root** on the fabd host:

```
sudo rm -rf -- {work-root}/run/{attempt}/claude-config {work-root}/run/{attempt}/home {work-root}/run/{attempt}/tmp
```

The raw streams under `{run area}/streams/{attempt}/` are fabd-only (0600). Keep them for review, or remove them the same way. Then pass `--ack-unswept`. The release refuses these holds without it.

For the redaction classes, the sweep ran and its counts are in `{run area}/streams/{attempt}/sweep.json`. Check any `credential_exposure` record before you release.

## 3. Release

```
fab mission release --mission M --reason "one line, why" --work-root {work-root} [--run-area {run area}] [--ack-unswept]
```

What the release checks before it changes anything:

- The reason must be one line of printable ASCII, at most 200 bytes.
- A reason that looks like a credential is refused. Never paste a token, key or DSN into it.
- The records directory must exist. If it does not, the release refuses and the hold stays.

The release then runs in one database transaction, in this order:

1. It locks the hold row and reads its generation G.
2. It writes the `release_authorized` record, holding the mission, attempt, G, reason and ack. The write is durable: temporary file, fsync, rename, directory fsync.
3. It deletes the row only if its generation is still G, then commits.

If step 2 fails, the transaction rolls back and the hold stays. A `release_authorized` record whose row still exists means the release was interrupted before commit. Run the same command again: it records G once and releases.

A hold placed while a release is in flight waits for that release to finish. It then becomes a new hold with a new generation, which the earlier release cannot delete.

Lock order is always the database row lock first, then the records file lock.

## 4. A wedged mission: `prior_attempt_unsettled`

On the token path, fabd refuses START while any earlier attempt of the mission has a binding record (`bindings/{attempt}.json`) and no `.settled` marker. **Releasing the operator hold does not lift this.**

It happens when a quarantined attempt's stop never verified after a fabd restart. Following AC-35(i), that attempt is held with `sweep_incomplete` and its material is kept.

The path out:

1. On the host, verify that the attempt's worker unit is gone from the worker manager's unit listing (the listing W1's verified stop reads, AC-4). Nothing of that attempt may still run.
2. Restart fabd, or wait for the prober. Once W1's verified stop returns Verified, fabd runs the same settle sequence: redact, gate, sweep, records, MarkSettled, GC. The `.settled` marker then exists and the refusal lifts.
3. **If the stop never verifies, stop and escalate.** Do not write a `.settled` marker by hand, and do not delete the binding record. No flag bypasses the settled marker, because the marker is what lets GC drop the token version.

<a id="bindings-lost"></a>

## 5. Bindings lost

Each M1 START records, in the same database transaction, that the attempt runs on the token path. On every scan, restart recovery visits every such attempt that has no `.settled` marker. It also visits every binding record in `{run area}/bindings/`.

An attempt that launched (it has `ATTEMPT_ACKED` and no prelaunch FINISH) but whose binding record is gone has lost its bound material. Nothing can be swept for it. fabd then does three things, in this order:

1. It records `sweep_incomplete` with class `material_unavailable`.
2. It places the `credential_material_unavailable` hold and writes its `mission_hold` record.
3. Only after the hold commits does it write the `.settled` marker.

Until then, START of that mission is refused with `prior_attempt_unsettled`.

**If the whole bindings directory is lost or emptied, every M1 attempt on the host is held.** The settled markers are gone too, so even attempts that were already settled become owed. This fails closed, and the operator releases each mission.

fabd alerts on this when one scan finds two or more lost bindings, or finds the directory itself missing (it recreates it, 0700, as it does at start). It never exits. The alert has three parts:

- one `bindings_lost` record in `{run area}/records/`. It holds counts only: `attempts` and `missions` held;
- one stderr line for that scan:
  `fabd: ALERT bindings directory lost or emptied; N M1 missions held; see docs/m1-runbook.md#bindings-lost`;
- `fab status --work-root {work-root}`, which lists `operator holds: N` and each held mission's class and attempt, followed by `ALERT bindings lost at <time>: A M1 attempts, M missions held; ...`.

A single lost binding is still held. It writes no `bindings_lost` record and no ALERT line.

Steps, each with its expected output:

1. **Confirm that no worker unit runs.** Restart recovery's verified stop (VerifyGone) is one way. The other, as root on the fabd host:

   ```
   systemctl --user -M fab-worker@ list-units 'fab-*'
   ```

   Expected: `0 loaded units listed.` If any unit is listed, stop here and escalate. Nothing may be released while a worker runs.

2. **Find the affected missions and attempts:**

   ```
   fab status --work-root {work-root}
   ```

   Expected: `operator holds: N`, then one line per mission ending in `class: credential_material_unavailable  attempt: r_...`, then the `ALERT bindings lost` line.

3. **Remove each affected attempt's run roots, as root:**

   ```
   sudo rm -rf -- {work-root}/run/{attempt}/claude-config {work-root}/run/{attempt}/home {work-root}/run/{attempt}/tmp
   ```

   Expected: no output, exit 0, and `ls {work-root}/run/{attempt}` no longer lists those three directories.

4. **Release each mission:**

   ```
   fab mission release --mission M --reason "bindings lost: <ticket>" --ack-unswept --work-root {work-root}
   ```

   Expected: exit 0, and `fab status` no longer lists M. Without `--ack-unswept` the release refuses and the hold stays. A restarted fabd does not hold M again, because the attempt's `.settled` marker was written after its hold.

## Limitations

- **Raw SQL delete.** Any app-role database session can delete the hold row with raw SQL and skip the record. The worker holds no database credentials, so this is an operator-side gap, not a worker one.
- **Generation visibility.** The export role can read `outbox_cursors.last_id`, so it can see a hold's generation. That number is not a secret.
