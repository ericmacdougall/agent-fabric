#!/bin/sh
# The recorded fake (fakeclaude.sh) for registry dispatch (goodqa#15373 D1
# AC-9): the prompt must arrive on stdin as exactly one stream-json user
# message whose content is the projection of this mission (FAB_MISSION).
# Only then does it run the recorded fake's script; otherwise it ends blocked
# with "stdin prompt missing or wrong", so a broken stdin path can never look
# like a finished run. It never reads the prompt from argv.
#
# The JSON checks are textual on purpose: fabd writes compact json.Marshal
# output (no spaces), and the worker sandbox may have no JSON tool.
set -u
HERE=$(cd "$(dirname "$0")" && pwd)
SID="00000000-0000-4000-8000-$(printf '%012d' $(( $(date +%s) % 1000000000000 )))"
fail() {
  printf '{"type":"system","subtype":"init","session_id":"%s","model":"fake","tools":[],"mcp_servers":[],"plugins":[],"claude_code_version":"fake-stdin-1"}\n' "$SID"
  printf '{"type":"result","subtype":"success","session_id":"%s","uuid":"r0","num_turns":1,"total_cost_usd":0,"is_error":false,"structured_output":{"next":"fix the dispatch stdin","end":{"kind":"blocked","reason":"stdin prompt missing or wrong: %s"}}}\n' "$SID" "$1"
  exit 0
}
in_stream=0
for a in "$@"; do
  [ "$a" = "--input-format" ] && in_stream=1
done
[ "$in_stream" = 1 ] || fail "not launched with --input-format stream-json"
[ -n "${FAB_MISSION:-}" ] || fail "no FAB_MISSION"
IN=$(cat)
lines=$(printf '%s\n' "$IN" | grep -c .)
[ "$lines" = 1 ] || fail "want 1 stdin message, got $lines"
case "$IN" in
  *'"type":"user"'*) ;;
  *) fail "stdin message is not a user message";;
esac
case "$IN" in
  *'\"mission\":\"'"${FAB_MISSION:-}"'\"'*) ;;
  *) fail "stdin prompt is not this mission's projection";;
esac
# the recorded fake sits beside this script (repo layout) or as fakeclaude-live (t2-dev)
for d in "$HERE/fakeclaude.sh" "$HERE/fakeclaude-live"; do
  [ -x "$d" ] && exec "$d"
done
fail "recorded fake not found beside $0"
