#!/usr/bin/env python3
"""Must-fail mutants for the fabric reducer and store.

Each mutant in mutants.json is a real source edit applied to a scratch copy of
the repository (never to the checkout itself). The package's tests then run
with a timeout; the mutant is KILLED only if the run fails AND the named test is
among the failures. A SURVIVED or BUILD_FAIL mutant fails the gate: a mutant
that does not compile proves nothing about the tests.

Usage: python3 -I tools/mutants/run.py --repo <checkout> --out <tsv> [--only M1,M2]
       [--scratch <dir>] [--timeout 240] [--env KEY=VALUE ...]
"""
import argparse
import json
import os
import re
import shutil
import subprocess
import sys
import time


def apply_edits(root, file, edits):
    path = os.path.join(root, file)
    with open(path, encoding="utf-8") as f:
        src = f.read()
    for old, new in edits:
        n = src.count(old)
        if n != 1:
            raise RuntimeError(f"{file}: edit target occurs {n} times, need exactly 1: {old[:60]!r}")
        src = src.replace(old, new, 1)
    with open(path, "w", encoding="utf-8") as f:
        f.write(src)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--repo", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--only", default="")
    ap.add_argument("--scratch", default=os.path.expanduser("~/fabric-dev/mut"))
    ap.add_argument("--timeout", type=int, default=240)
    ap.add_argument("--env", action="append", default=[])
    args = ap.parse_args()

    repo = os.path.abspath(args.repo)
    with open(os.path.join(repo, "tools", "mutants", "mutants.json"), encoding="utf-8") as f:
        mutants = json.load(f)
    only = {m for m in args.only.split(",") if m}
    env = dict(os.environ)
    for kv in args.env:
        k, _, v = kv.partition("=")
        env[k] = v

    # Clean baseline first: the unmutated suites must pass, or no verdict means anything.
    pkgs = sorted({m["pkg"] for m in mutants if not only or m["id"] in only or m["id"].split("_")[0] in only})
    base = subprocess.run(["go", "test", *pkgs, "-count=1", "-timeout", f"{args.timeout}s"], cwd=repo, env=env, capture_output=True, text=True)
    if base.returncode != 0:
        print("BASELINE FAIL: the unmutated suites do not pass; no mutant can be judged", flush=True)
        print((base.stdout + base.stderr)[-2000:])
        sys.exit(2)
    print(f"BASELINE PASS: {' '.join(pkgs)}", flush=True)
    os.makedirs(args.scratch, exist_ok=True)
    os.makedirs(os.path.dirname(os.path.abspath(args.out)), exist_ok=True)
    rows = []
    gate_ok = True
    for m in mutants:
        if only and m["id"] not in only and m["id"].split("_")[0] not in only:
            continue
        work = os.path.join(args.scratch, m["id"])
        shutil.rmtree(work, ignore_errors=True)
        shutil.copytree(repo, work, ignore=shutil.ignore_patterns(".git", ".local"))
        t0 = time.time()
        try:
            apply_edits(work, m["file"], m["edits"])
        except RuntimeError as e:
            rows.append((m["id"], m["invariant"], m["expect"], "INVALID_EDIT", "-", "0.0", str(e)))
            print(f"{m['id']}\tINVALID_EDIT\t{e}", flush=True)
            gate_ok = False
            continue
        cmd = ["go", "test", m["pkg"], "-count=1", "-timeout", f"{args.timeout}s"]
        try:
            p = subprocess.run(cmd, cwd=work, env=env, capture_output=True, text=True, timeout=args.timeout + 30)
            out = p.stdout + p.stderr
            code = p.returncode
        except subprocess.TimeoutExpired as e:
            out = (e.stdout or "") + (e.stderr or "")
            code = -1
        secs = f"{time.time() - t0:.1f}"
        failed = set(re.findall(r"^--- FAIL: (\w+)", out, re.M))
        if "build failed" in out or "cannot use" in out or "undefined:" in out or "declared and not used" in out:
            outcome = "BUILD_FAIL"
            gate_ok = False
        elif code == 0:
            outcome = "SURVIVED"
            gate_ok = False
        elif code == -1:
            outcome = "TIMEOUT"
            gate_ok = False
        elif m["expect"] in failed:
            outcome = "KILLED"
        else:
            outcome = "KILLED_OTHER"  # failed, but not by the test named for this invariant
            gate_ok = False
        rows.append((m["id"], m["invariant"], m["expect"], outcome, str(code), secs, ",".join(sorted(failed))))
        shutil.rmtree(work, ignore_errors=True)
        print(f"{m['id']}\t{outcome}\texit={code}\t{secs}s\tfailed={','.join(sorted(failed))}", flush=True)

    with open(args.out, "w", encoding="utf-8") as f:
        f.write("id\tinvariant\texpect\toutcome\texit\tseconds\tfailed_tests\n")
        for r in rows:
            f.write("\t".join(r) + "\n")
    print(f"GATE {'PASS' if gate_ok else 'FAIL'}: {sum(1 for r in rows if r[3] == 'KILLED')}/{len(rows)} killed -> {args.out}")
    sys.exit(0 if gate_ok else 1)


if __name__ == "__main__":
    main()
