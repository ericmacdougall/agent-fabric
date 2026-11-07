#!/usr/bin/env python3
"""Pre-commit guard: refuse a commit when any source file holds a mutant's replacement
text instead of its original (a mutant run left a file mutated). Usage: check_clean.py <repo> <spec.json>..."""
import json, os, sys
repo = sys.argv[1]
bad = 0
for spec in sys.argv[2:]:
    for m in json.load(open(spec)):
        p = os.path.join(repo, m["file"])
        if not os.path.exists(p):
            continue
        t = open(p).read()
        if m["new"] and m["new"] in t and m["old"] not in t:
            print("MUTATED:", m["id"], m["file"]); bad += 1
sys.exit(1 if bad else 0)
