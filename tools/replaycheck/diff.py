#!/usr/bin/env python3
"""Compare two replaycheck TSVs: the old reader (built at the live commit) and
the new reader (built at the candidate). Refuses when either file lacks its
`# build <commit>` line or when both name the same commit (the comparison would
prove nothing). A mission that already mismatches on the old side is reported,
not counted as a regression. Exit 0 only when every mission matches.

usage: python3 -I tools/replaycheck/diff.py old.tsv new.tsv
"""
import sys


def load(path):
    commit, rows = None, {}
    with open(path) as f:
        for line in f:
            line = line.rstrip("\n")
            if line.startswith("# build "):
                commit = line[len("# build "):].strip()
            elif line and not line.startswith("#") and not line.startswith("mission\t"):
                parts = line.split("\t")
                if len(parts) != 4:
                    sys.exit("malformed row in %s: %r" % (path, line[:60]))
                rows[parts[0]] = tuple(parts[1:])
    return commit, rows


def main():
    if len(sys.argv) != 3:
        sys.exit(__doc__)
    oc, old = load(sys.argv[1])
    nc, new = load(sys.argv[2])
    if not oc or not nc or oc == "unknown" or nc == "unknown":
        sys.exit("refused: each TSV must record the commit it was built from")
    if oc == nc:
        sys.exit("refused: both TSVs were built from the same commit %s" % oc)
    bad = 0
    for m in sorted(set(old) | set(new)):
        o, n = old.get(m), new.get(m)
        if o == n:
            continue
        pre = o is not None and (o[1] != "true" or o[2] != "ok")
        print("%s\told=%s\tnew=%s%s" % (m, o, n, "\t(already mismatching on the old side)" if pre else ""))
        if not pre:
            bad += 1
    print("old %s new %s: %d missions old, %d new, %d regressions" % (oc[:12], nc[:12], len(old), len(new), bad))
    sys.exit(1 if bad else 0)


if __name__ == "__main__":
    main()
