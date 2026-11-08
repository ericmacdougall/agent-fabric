"""Generate internal/ladder/testdata/parity.json from the harness ladder.

The expected values come from running escalation.py's own ladder_status on
synthetic, seeded inputs, so the Go port is checked against the reference
implementation rather than against a re-statement of it (goodqa#15324).

Usage: python3 -I tools/ladder-parity/gen.py HOOKS_DIR OUT_JSON [SEED]

HOOKS_DIR is the directory holding escalation.py and model_tiers.py. No
real ledger is read; every input is generated here.
"""
import hashlib
import json
import os
import random
import sys


def policies():
    base = {
        "lowest_rung": "R0",
        "failures_per_rung": 2,
        "ladder_limits": {"max_review_launches": 8, "max_rung_climbs": 3, "max_total_review_usd": 60},
        "rungs": {
            "R0": {"writable": True, "needs_user_permission": False},
            "R1": {"writable": True, "needs_user_permission": False},
            "R2": {"writable": True, "needs_user_permission": False},
            "R3": {"writable": True, "needs_user_permission": False},
            "R4": {"writable": False, "needs_user_permission": False},
            "R5": {"writable": True, "needs_user_permission": True},
            "R6": {"writable": False, "needs_user_permission": True},
        },
    }
    tight = json.loads(json.dumps(base))
    tight["failures_per_rung"] = 1
    tight["ladder_limits"] = {"max_review_launches": 2, "max_rung_climbs": 1, "max_total_review_usd": 0.3}
    tight["rungs"]["R2"]["needs_user_permission"] = True
    return {"base": base, "tight": tight}


def event(rng):
    cls = rng.choice(["FAIL", "FAIL", "FAIL", "CLAIM", "PASS", "NOT_COUNTED"])
    return {
        "class": cls,
        "fingerprint": rng.choice([None, "fa", "fa", "fb", "fc"]),
        "head": rng.choice([None, "", "h1", "h1", "h2", "h3"]),
    }


def handmade():
    f = lambda fp, head: {"class": "FAIL", "fingerprint": fp, "head": head}
    claim = {"class": "CLAIM", "fingerprint": None, "head": None}
    return {
        "empty": [],
        "climb_after_two": [f("fa", "h1"), f("fa", "h2")],
        "new_fingerprint_resets": [f("fa", "h1"), f("fb", "h2"), f("fa", "h3")],
        "same_head_no_claim_skipped": [f("fa", "h1"), f("fa", "h1")],
        "same_head_after_claim_counts": [f("fa", "h1"), claim, f("fa", "h1")],
        "empty_head_never_skipped": [f("fa", ""), f("fa", "")],
        "null_head_never_skipped": [f("fa", None), f("fa", None)],
        "ignored_classes": [{"class": "PASS", "fingerprint": "fa", "head": "h1"},
                            {"class": "NOT_COUNTED", "fingerprint": None, "head": None}],
        "top_stays": [f("f%d" % i, "h%d" % i) for i in range(12)],
        "climb_to_top_and_stay": [f("fa", "h%d" % i) for i in range(14)],
        "null_fingerprint_is_a_fingerprint": [f(None, "h1"), f(None, "h2")],
    }


def main(argv):
    hooks, out = argv[1], argv[2]
    seed = int(argv[3]) if len(argv) > 3 else 15324
    sys.path.insert(0, hooks)
    import escalation  # noqa: E402

    rng = random.Random(seed)
    sequences = dict(handmade())
    for i in range(40):
        sequences["random_%02d" % i] = [event(rng) for _ in range(rng.randint(0, 14))]
    spends = [0, 0.1, 0.30000000000000004, 0.3, 59.99, 60, 60.0, 75.5]
    vectors = []
    for pname, policy in sorted(policies().items()):
        for sname, events in sorted(sequences.items()):
            state = {
                "task": "synthetic-" + sname,
                "events": events,
                "launches": rng.choice([0, 1, 2, 7, 8, 9]),
                "spend_usd": rng.choice(spends),
                "permission": None,
            }
            got = escalation.ladder_status(state, policy)
            vectors.append({
                "name": pname + "/" + sname,
                "policy": pname,
                "state": state,
                "want": {k: got[k] for k in ("rung", "failures_at_rung", "fingerprint", "climbs",
                                             "blocked_by", "needs_user_permission", "task")},
            })
    # Record which reference produced the expected values, so a
    # regeneration against a changed escalation.py shows up in review.
    reference = {}
    for name in ("escalation.py", "model_tiers.py"):
        with open(os.path.join(hooks, name), "rb") as src:
            reference[name] = hashlib.sha256(src.read()).hexdigest()
    with open(out, "w") as handle:
        json.dump({"seed": seed, "reference_sha256": reference, "policies": policies(), "vectors": vectors},
                  handle, indent=1, sort_keys=True)
        handle.write("\n")
    print("wrote %d vectors (seed %d)" % (len(vectors), seed))


if __name__ == "__main__":
    main(sys.argv)
