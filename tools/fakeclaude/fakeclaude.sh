#!/bin/sh
# A recorded-shape provider for shadow runs: speaks claude's stream-json
# (system/init, assistant usage, result with structured_output) without a
# model, a login or a network. Its behaviour per mission is a tiny state
# machine in the worktree so the daemon's wait/wake path can be exercised:
#   run 1: start a host job (sleep N) and end with wait job:<id>
#   run 2: end with blocked (the fake has nothing more to do; a real worker
#          would checkpoint and be relaunched until ready_for_review)
# Env from the launch: FAB_RUN, FAB_MISSION, FAB_ATTEMPT, FAB_FENCE, FAB_RUN_DIR,
# FAB_WORKTREE, FABRIC_DSN, FABRIC_HOME. The fab binary must be on PATH or in FAB_BIN.
set -u
FAB=${FAB_BIN:-fab}
JOB_SECONDS=${FAKE_JOB_SECONDS:-5}
STATE="${FAB_WORKTREE:-.}/.fakeclaude.state"
SID="00000000-0000-4000-8000-$(printf '%012d' $(( $(date +%s) % 1000000000000 )))"
printf '{"type":"system","subtype":"init","session_id":"%s","model":"fake","tools":[],"mcp_servers":[],"plugins":[],"claude_code_version":"fake-1"}\n' "$SID"
if [ ! -f "$STATE" ]; then
  echo "started" > "$STATE"
  OUT=$("$FAB" job run --rundir "$FAB_RUN_DIR" -- sleep "$JOB_SECONDS" 2>&1)
  JOB=$(printf '%s' "$OUT" | sed -n 's/^job \([^ ]*\) STARTED.*/\1/p')
  printf '{"type":"assistant","session_id":"%s","uuid":"u1","message":{"usage":{"input_tokens":12,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":20}}}\n' "$SID"
  if [ -z "$JOB" ]; then
    printf '{"type":"result","subtype":"success","session_id":"%s","uuid":"r1","num_turns":1,"total_cost_usd":0,"is_error":false,"structured_output":{"next":"job failed to start","end":{"kind":"blocked","reason":"%s"}}}\n' "$SID" "$(printf '%s' "$OUT" | tr -d '"\n' | cut -c1-150)"
    exit 0
  fi
  printf '{"type":"result","subtype":"success","session_id":"%s","uuid":"r1","num_turns":1,"total_cost_usd":0,"is_error":false,"structured_output":{"next":"wait for the job","end":{"kind":"wait","event":"job:%s"}}}\n' "$SID" "$JOB"
  exit 0
fi
echo "done" > "$STATE"
printf '{"type":"assistant","session_id":"%s","uuid":"u2","message":{"usage":{"input_tokens":14,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":9}}}\n' "$SID"
printf '{"type":"result","subtype":"success","session_id":"%s","uuid":"r2","num_turns":1,"total_cost_usd":0,"is_error":false,"structured_output":{"next":"nothing","end":{"kind":"blocked","reason":"fake provider: scripted end"}}}\n' "$SID"
