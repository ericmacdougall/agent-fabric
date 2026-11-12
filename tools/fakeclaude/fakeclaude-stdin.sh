#!/bin/sh
# The recorded fake for registry dispatch (goodqa#15373 D1 AC-9). The prompt
# must arrive on stdin, read from fd 0 (never by reopening /dev/stdin), as
# exactly one stream-json user message whose content is a projection with
# .checkpoint.mission equal to FAB_MISSION. Only then does it exec the
# recorded fake beside it, and only if that file has the recorded sha256
# (FAKE_SHA in tools/fabd-install/install.sh). Otherwise it ends blocked with
# "stdin prompt missing or wrong", so a broken stdin path can never look like
# a finished run. It never reads the prompt from argv.
#
# A mission id starting m_d1sleep_ sleeps 120 s before delegating, so the
# rehearsal can stop fabd with an attempt in flight (NEG-10).
# Needs jq and sha256sum (or shasum) inside the worker sandbox; a missing
# tool fails closed into the blocked end.
set -u
DELEGATE_SHA=50d4f1d864de7b2b2ab38808d4414a3adf8a2c4b0fc0047571bdb515689d5285
HERE=$(cd "$(dirname "$0")" && pwd)
SID="00000000-0000-4000-8000-$(printf '%012d' $(( $(date +%s) % 1000000000000 )))"
fail() {
  printf '{"type":"system","subtype":"init","session_id":"%s","model":"fake","tools":[],"mcp_servers":[],"plugins":[],"claude_code_version":"fake-stdin-2"}\n' "$SID"
  printf '{"type":"result","subtype":"success","session_id":"%s","uuid":"r0","num_turns":1,"total_cost_usd":0,"is_error":false,"structured_output":{"next":"fix the dispatch stdin","end":{"kind":"blocked","reason":"stdin prompt missing or wrong: %s"}}}\n' "$SID" "$1"
  exit 0
}
sha() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1
  elif command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | cut -d' ' -f1
  fi
}
in_stream=0
for a in "$@"; do
  [ "$a" = "--input-format" ] && in_stream=1
done
[ "$in_stream" = 1 ] || fail "not launched with --input-format stream-json"
[ -n "${FAB_MISSION:-}" ] || fail "no FAB_MISSION"
command -v jq >/dev/null 2>&1 || fail "no jq in the worker sandbox"
IN=$(cat)
lines=$(printf '%s\n' "$IN" | grep -c .)
[ "$lines" = 1 ] || fail "want 1 stdin message, got $lines"
content=$(printf '%s\n' "$IN" | jq -er 'select(.type == "user") | .message.content | strings' 2>/dev/null) \
  || fail "stdin message is not a user message with string content"
mission=$(printf '%s\n' "$content" | jq -er '.checkpoint.mission | strings' 2>/dev/null) \
  || fail "stdin content has no checkpoint mission"
[ "$mission" = "$FAB_MISSION" ] || fail "stdin prompt is not this mission's projection"
# the recorded fake: fakeclaude-live (the live layout) first, then fakeclaude.sh (repo layout)
d=""
for c in "$HERE/fakeclaude-live" "$HERE/fakeclaude.sh"; do
  if [ -f "$c" ]; then d=$c; break; fi
done
[ -n "$d" ] || fail "recorded fake not found beside $0"
[ -x "$d" ] || fail "recorded fake $d is not executable"
[ "$(sha "$d")" = "$DELEGATE_SHA" ] || fail "recorded fake $d does not have the recorded sha256"
case "$mission" in
  m_d1sleep_*) sleep 120;;
esac
exec "$d"
