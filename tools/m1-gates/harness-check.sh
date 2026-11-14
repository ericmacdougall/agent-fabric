#!/usr/bin/env bash
# goodqa#15966 AC-30: the pre-mission harness check of the pinned claude CLI
# with the AC-14 environment. Run it on fabric-node-1 before any mission.
#
#   harness-check.sh --claude /opt/claude/claude --sha <64-hex> \
#                    --token-file /srv/fabric/secret/claude-oauth [--keep]
#
# It confirms, printing counts only:
#   1. a Bash tool child's environment lacks CLAUDE_CODE_OAUTH_TOKEN (the
#      child prints only whether the variable name is present);
#   2. no transcript file is written under CLAUDE_CONFIG_DIR;
#   3. the CLI started in a read-only working directory (an AC-23 style
#      extract: files 0444/0555, directories 0555) completes a turn without
#      error;
#   4. the token value appears in no output the run produced.
# Exit 0: all hold. Exit 1: a check failed. Exit 2: usage or environment
# error. The token is read into the claude process's own environment only;
# it is never in argv, a log or this script's output.
set -uo pipefail
set +x

usage() { echo "usage: $0 --claude PATH --sha HEX64 --token-file FILE [--keep]" >&2; exit 2; }
CLAUDE= SHA= TOKEN_FILE= KEEP=0
while [ $# -gt 0 ]; do
  case "$1" in
    --claude) CLAUDE=${2:-}; shift 2 ;;
    --sha) SHA=${2:-}; shift 2 ;;
    --token-file) TOKEN_FILE=${2:-}; shift 2 ;;
    --keep) KEEP=1; shift ;;
    *) usage ;;
  esac
done
[ -n "$CLAUDE" ] && [ -n "$SHA" ] && [ -n "$TOKEN_FILE" ] || usage
[[ "$SHA" =~ ^[0-9a-f]{64}$ ]] || { echo "harness-check: --sha must be 64 lowercase hex" >&2; exit 2; }
[ -x "$CLAUDE" ] && [ ! -L "$CLAUDE" ] || { echo "harness-check: $CLAUDE is not an executable regular file" >&2; exit 2; }
[ -r "$TOKEN_FILE" ] && [ ! -L "$TOKEN_FILE" ] || { echo "harness-check: token file unreadable or a symlink" >&2; exit 2; }
got=$(sha256sum "$CLAUDE" 2>/dev/null | cut -d' ' -f1)
[ -z "$got" ] && got=$(shasum -a 256 "$CLAUDE" | cut -d' ' -f1)
[ "$got" = "$SHA" ] || { echo "harness-check: claude sha256 does not match the pin" >&2; exit 2; }

RUN=$(mktemp -d "${TMPDIR:-/tmp}/fab-harness-check.XXXXXX") || exit 2
cleanup() { [ "$KEEP" = 1 ] || { chmod -R u+w "$RUN" 2>/dev/null; rm -rf "$RUN"; }; }
trap cleanup EXIT
mkdir -p "$RUN/claude-config" "$RUN/home" "$RUN/wt" "$RUN/tmp" "$RUN/cache" "$RUN/config/git"
cat > "$RUN/wt/probe.sh" <<'EOF'
#!/bin/sh
if [ -n "${CLAUDE_CODE_OAUTH_TOKEN+x}" ]; then echo TOKEN_VAR_PRESENT; else echo TOKEN_VAR_ABSENT; fi
EOF
echo "harness check canary" > "$RUN/wt/README"
chmod 0555 "$RUN/wt/probe.sh"; chmod 0444 "$RUN/wt/README"; chmod 0555 "$RUN/wt"

# The claude process gets the production launch environment
# (internal/provider/claude/launch.go Argv, host/linux_manager.go TMPDIR and
# credential.ClaudeEnv: the AC-14 fixed variables and a per-run HOME) plus
# the token, read from the file inside the subshell (never argv). Like the
# production argv it passes no --no-session-persistence: whether a
# transcript is written is what check 2 measures.
(
  cd "$RUN/wt" || exit 2
  for v in $(compgen -e); do
    case "$v" in PATH|LANG) ;; *) unset "$v" ;; esac
  done
  export LANG=${LANG:-C.UTF-8} HOME="$RUN/home" TMPDIR="$RUN/tmp" CLAUDE_CONFIG_DIR="$RUN/claude-config" \
    CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=1 CLAUDE_CODE_SKIP_PROMPT_HISTORY=1 \
    CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 DISABLE_TELEMETRY=1 DISABLE_ERROR_REPORTING=1 \
    DISABLE_AUTOUPDATER=1 CLAUDE_CODE_DISABLE_CLAUDE_MDS=1 \
    XDG_CACHE_HOME="$RUN/cache" XDG_CONFIG_HOME="$RUN/config" GIT_CONFIG_GLOBAL="$RUN/config/git/config"
  IFS= read -r CLAUDE_CODE_OAUTH_TOKEN < "$TOKEN_FILE" || [ -n "${CLAUDE_CODE_OAUTH_TOKEN:-}" ]
  export CLAUDE_CODE_OAUTH_TOKEN
  exec "$CLAUDE" -p "Run ./probe.sh with the Bash tool, then reply with exactly its output." \
    --output-format stream-json --verbose --max-turns 3 --exclude-dynamic-system-prompt-sections --setting-sources "" \
    --tools Bash --allowedTools "Bash(./probe.sh)" --permission-mode default
) > "$RUN/stream.jsonl" 2> "$RUN/stderr.log"
code=$?

present=$(grep -c TOKEN_VAR_PRESENT "$RUN/stream.jsonl" "$RUN/stderr.log" | awk -F: '{s+=$2} END {print s+0}')
absent=$(grep -c TOKEN_VAR_ABSENT "$RUN/stream.jsonl" | awk -F: '{s+=$NF} END {print s+0}')
transcripts=$(find "$RUN/claude-config" "$RUN/home" -type f \( -name '*.jsonl' -o -path '*/projects/*' -o -path '*/transcripts/*' \) | wc -l | tr -d ' ')
result=$(grep '"type":"result"' "$RUN/stream.jsonl" | tail -1)
ok_result=0
case "$result" in *'"subtype":"success"'*'"is_error":false'*|*'"is_error":false'*'"subtype":"success"'*) ok_result=1 ;; esac
# The token never appears in any output: compared through a pattern file,
# so the value is not on any command line.
leaks=$( (IFS= read -r t < "$TOKEN_FILE"; printf '%s\n' "$t") | grep -F -c -f - "$RUN/stream.jsonl" "$RUN/stderr.log" 2>/dev/null | awk -F: '{s+=$NF} END {print s+0}')
cfgleaks=$( (IFS= read -r t < "$TOKEN_FILE"; printf '%s\n' "$t") | grep -r -F -l -f - "$RUN/claude-config" "$RUN/home" 2>/dev/null | wc -l | tr -d ' ')

echo "harness-check: exit=$code result_success=$ok_result token_var_present=$present token_var_absent=$absent transcripts=$transcripts token_in_output=$leaks token_in_config_files=$cfgleaks"
fail=0
[ "$code" -eq 0 ] || { echo "FAIL: claude exited $code"; fail=1; }
[ "$ok_result" -eq 1 ] || { echo "FAIL: no successful result event (read-only working directory turn)"; fail=1; }
[ "$present" -eq 0 ] || { echo "FAIL: a Bash tool child saw CLAUDE_CODE_OAUTH_TOKEN"; fail=1; }
[ "$absent" -ge 1 ] || { echo "FAIL: the probe did not run"; fail=1; }
[ "$transcripts" -eq 0 ] || { echo "FAIL: transcript files under CLAUDE_CONFIG_DIR"; fail=1; }
[ "$leaks" -eq 0 ] && [ "$cfgleaks" -eq 0 ] || { echo "FAIL: the token value appears in run output"; fail=1; }
[ "$fail" -eq 0 ] && echo "PASS"
exit $fail
