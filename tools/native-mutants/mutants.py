#!/usr/bin/env python3
"""Mutant runner: python3 mutants.py <srcdir> <spec.json>. Each mutant patches one
file, runs the named tests, requires failure, restores the file. Bounded 120s each."""
import json, subprocess, sys, shutil, os
src, spec = sys.argv[1], json.load(open(sys.argv[2]))
# goodqa#15299: FAB_GO names the go binary (default: go on PATH). Its
# directory goes first on PATH so the spec's "go test ..." commands use it
# too, and GOTOOLCHAIN=local stops go.mod from selecting another toolchain.
GO = os.environ.get("FAB_GO", "go")
ENV = dict(os.environ, GOTOOLCHAIN="local")
if os.path.isabs(GO):
    ENV["PATH"] = os.path.dirname(GO) + os.pathsep + ENV.get("PATH", "")
bad = 0
for m in spec:
    p = os.path.join(src, m["file"]); orig = open(p).read()
    if m["old"] not in orig:
        print("MUTANT-NOT-APPLICABLE", m["id"]); bad += 1; continue
    open(p, "w").write(orig.replace(m["old"], m["new"], 1))
    try:
        b = subprocess.run([GO, "build", "./internal/...", "./cmd/..."], cwd=src, env=ENV, capture_output=True, text=True, timeout=120)
        if b.returncode != 0:
            print("BUILD-BROKEN (invalid mutant)", m["id"], b.stderr[:200]); bad += 1; open(p, "w").write(orig); continue
        r = subprocess.run(m["cmd"], shell=True, cwd=src, env=ENV, capture_output=True, text=True, timeout=120)
        killed = r.returncode != 0
    except subprocess.TimeoutExpired:
        killed = True
    finally:
        open(p, "w").write(orig)
    print(("KILLED  " if killed else "SURVIVED"), m["id"]); bad += (not killed)
sys.exit(1 if bad else 0)
