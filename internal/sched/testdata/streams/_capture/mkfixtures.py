#!/usr/bin/env python3
"""Writes internal/sched/testdata/streams/<case>/{stream.jsonl,exit} under the tree given as argv[1].
Fixtures for goodqa#15373 AC-5 (one per settleCommand branch). Deterministic."""
import json, os, shutil, sys

root = os.path.join(sys.argv[1], "internal/sched/testdata/streams")
SID = "00000000-0000-4000-8000-000000000001"


def init():
    return {"type": "system", "subtype": "init", "session_id": SID, "model": "fake-model", "tools": ["Read"],
            "mcp_servers": [], "plugins": [], "claude_code_version": "0.0.0-fixture"}


def assistant(ctx=1000):
    return {"type": "assistant", "session_id": SID,
            "message": {"usage": {"input_tokens": 3, "cache_creation_input_tokens": ctx, "cache_read_input_tokens": 7,
                                  "output_tokens": 11}}}


def result(subtype="success", is_error=False, structured=None, turns=2, cost=0.0125):
    r = {"type": "result", "subtype": subtype, "is_error": is_error, "session_id": SID, "num_turns": turns,
         "total_cost_usd": cost, "stop_reason": "end_turn", "uuid": "00000000-0000-4000-8000-0000000000aa",
         "permission_denials": []}
    if structured is not None:
        r["structured_output"] = structured
    return r


def end(kind, event=None):
    e = {"kind": kind}
    if event:
        e["event"] = event
    return {"end": e}


CASES = {
    "wait": ([init(), assistant(), result(structured=end("wait", "job:j_fixture"))], 0),
    "checkpoint": ([init(), assistant(), result(structured=end("checkpoint"))], 0),
    "ready_for_review": ([init(), assistant(), result(structured=end("ready_for_review"))], 0),
    "blocked": ([init(), assistant(), result(structured=end("blocked"))], 0),
    "unknown_end_kind": ([init(), assistant(), result(structured=end("frobnicate"))], 0),
    "checkpoint_without_end_kind": ([init(), assistant(), result(structured={"next": "x"})], 0),
    "no_structured_output": ([init(), assistant(), result()], 0),
    "result_failure": ([init(), assistant(), result(subtype="error_max_turns", is_error=True)], 0),
    "exit_nonzero_with_success": ([init(), assistant(), result(structured=end("checkpoint"))], 1),
    "contradictory": ([init(), init(), assistant(), result(structured=end("checkpoint"))], 0),
    "no_result_event": ([init(), assistant()], 0),
}

if os.path.exists(root):
    shutil.rmtree(root)
os.makedirs(root)
for name, (events, code) in CASES.items():
    d = os.path.join(root, name)
    os.makedirs(d)
    with open(os.path.join(d, "stream.jsonl"), "w") as f:
        for e in events:
            f.write(json.dumps(e, separators=(",", ":")) + "\n")
    open(os.path.join(d, "exit"), "w").write(f"{code}\n")
# e1_A: the recorded real-claude stream, exit 0
d = os.path.join(root, "e1_A")
os.makedirs(d)
shutil.copyfile(os.path.join(sys.argv[1], "internal/provider/claude/testdata/e1_A.jsonl"), os.path.join(d, "stream.jsonl"))
open(os.path.join(d, "exit"), "w").write("0\n")
# stream unreadable: stream.jsonl is a directory, so Open succeeds and the read fails (sched.go:1052-1054)
d = os.path.join(root, "stream_unreadable")
os.makedirs(os.path.join(d, "stream.jsonl"))
open(os.path.join(d, "stream.jsonl", ".keep"), "w").close()  # so git keeps the directory
open(os.path.join(d, "exit"), "w").write("0\n")
print(len(CASES) + 2, "cases")
