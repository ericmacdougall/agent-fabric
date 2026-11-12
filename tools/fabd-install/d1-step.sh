#!/bin/bash
# goodqa#15373 D1 AC-8: one registry-mode dispatch of one disposable mission
# through the pinned stdin fake, on a host whose fabd runs --no-dispatch, then
# the unit restored and checked. The rehearsal and the live step run this same
# script; only the arguments differ.
#
#   DB=<database> tools/fabd-install/d1-step.sh UNIT SVC FAB MISSION BIN_DIR
#
#   UNIT     the user unit (fabd, or fabd-rh for the rehearsal)
#   SVC      its service dir (env, guard.sh, fabd.lock); SVC/env is passed to
#            systemd by path and never read here
#   FAB      the fab binary the unit runs (deployed or branch binary)
#   MISSION  a new mission id, m_d1live_* / m_d1rh_* (m_d1sleep_* sleeps 120 s
#            in the fake, for NEG-10)
#   BIN_DIR  where the recorded fake (fakeclaude-live) is and the stdin fake
#            is installed (/srv/fabric/bin on live)
#
# Database access uses the administrative credential in ~/fabric-dev/test.env
# (as install.sh does), passed to each process by environment, never argv;
# every check runs read-only. Prints D1STEP key=value records; exit 0 only
# when every check passed. Steps follow spec v7 "Live procedure (AC-8)".
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../.." && pwd)
D=$HOME/fabric-dev
[[ $# == 5 ]] || { echo "usage: DB=<db> $0 UNIT SVC FAB MISSION BIN_DIR" >&2; exit 2; }
UNIT=$1 SVC=$2 FAB=$3 MISSION=$4 BIN=$5
: "${DB:?DB is required}"
for v in UNIT SVC FAB BIN DB; do
  [[ "${!v}" =~ ^[A-Za-z0-9/._-]+$ ]] || { echo "d1-step: $v has unsafe characters" >&2; exit 2; }
done
[[ "$MISSION" =~ ^m_d1(live|rh|sleep)_[a-z0-9_]{1,40}$ ]] || { echo "d1-step: MISSION must be m_d1live_*, m_d1rh_* or m_d1sleep_*" >&2; exit 2; }
STEP_UNIT=fabd-d1-step
POLL_S=${D1_POLL_S:-600}
QUIET_S=${D1_QUIET_S:-120}
rec() { echo "D1STEP $*"; }
fail() { rec "result=FAIL reason=\"$*\""; exit 3; }

ADMIN=$(set -a; . "$D/test.env" >/dev/null; printf '%s' "${FABRIC_TEST_DSN:-}")
[[ -n "$ADMIN" ]] || fail "test.env sets no FABRIC_TEST_DSN"
DSN=$(ADMIN="$ADMIN" python3 -I -c '
import os, sys, urllib.parse as u
p = u.urlsplit(os.environ["ADMIN"]); q = dict(u.parse_qsl(p.query)); q["application_name"] = "fabd-d1-step"
print(u.urlunsplit((p.scheme, p.netloc, "/" + sys.argv[1], u.urlencode(q), "")))' "$DB")
unset ADMIN
qr() { PGOPTIONS='-c default_transaction_read_only=on' PGCALL_DSN="$DSN" python3 -I "$D/pgcall.py" psql -X -At -v ON_ERROR_STOP=1 -c "$1"; }
fabw() { FABRIC_DSN="$DSN" FABRIC_ACTOR=op:d1step "$FAB" "$@"; }
sha() { sha256sum "$1" | cut -d' ' -f1; }
UF=$HOME/.config/systemd/user/$UNIT.service

# --- step 2: save the running unit's state
[[ -f "$UF" ]] || fail "no unit file $UF"
systemctl --user is-active --quiet "$UNIT" || fail "$UNIT is not active"
dropins() { systemctl --user cat "$UNIT" | grep -c "^# .*/$UNIT.service.d/" || true; }
[[ "$(dropins)" == 0 ]] || fail "$UNIT has a drop-in"
cmdline() {  # the fab daemon child of the flock wrapper (MainPID)
  local m c; m=$(systemctl --user show -p MainPID --value "$UNIT")
  c=$(pgrep -P "$m" | head -1); [[ -n "$c" ]] || c=$m
  tr '\0' '\n' < "/proc/$c/cmdline"
}
SAVED_SHA=$(sha "$UF")
SAVED_ARGV=$(cmdline)
SAVED_NRESTARTS=$(systemctl --user show -p NRestarts --value "$UNIT")
grep -qx -- '--no-dispatch' <<<"$SAVED_ARGV" || fail "$UNIT does not run --no-dispatch"
T0=$(qr "SELECT to_char(clock_timestamp() AT TIME ZONE 'UTC', 'YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"')")
LAUNCHED() { qr "SELECT COALESCE((SELECT value FROM sched_counters WHERE name = 'runs_launched_total'), 0)"; }
rec "step=2 unit=$UNIT unit_sha=$SAVED_SHA nrestarts=$SAVED_NRESTARTS runs_launched_total=$(LAUNCHED) t0=$T0 systemd=$(systemctl --version | head -1 | tr ' ' _) worker_user=$(grep -A1 -x -- '--worker-user' <<<"$SAVED_ARGV" | tail -n +2 | head -1)"

# --- step 3: the pinned stdin fake and its pinned delegate; worker tools
PIN=$(sed -n 's/^const StdinFakeSHA256 = "\([0-9a-f]\{64\}\)"$/\1/p' "$REPO/internal/dispatch/claudecode.go")
FAKE_SHA=$(sed -n 's/^FAKE_SHA=\([0-9a-f]\{64\}\)$/\1/p' "$HERE/install.sh")
[[ -n "$PIN" && -n "$FAKE_SHA" ]] || fail "could not read the pins from the repo"
[[ "$(sha "$BIN/fakeclaude-live")" == "$FAKE_SHA" ]] || fail "$BIN/fakeclaude-live is not the recorded fake"
[[ ! -e "$BIN/fakeclaude.sh" ]] || fail "$BIN/fakeclaude.sh exists; the stdin fake would not be unambiguous"
install -m 0755 "$REPO/tools/fakeclaude/fakeclaude-stdin.sh" "$BIN/fakeclaude-stdin"
[[ "$(sha "$BIN/fakeclaude-stdin")" == "$PIN" ]] || fail "the installed stdin fake does not hash to the pin"
for t in jq sha256sum; do PATH=/usr/bin:/bin command -v "$t" >/dev/null || fail "$t missing for the worker"; done
rec "step=3 stdin_fake=$BIN/fakeclaude-stdin pin=$PIN delegate_sha=$FAKE_SHA tools=jq,sha256sum"

# --- step 4: the disposable mission, and the hard gate
case "$MISSION" in m_d1live_*) NAME=d1-live-fake;; m_d1sleep_*) NAME=d1-sleep-rh;; *) NAME=d1-rh-fake;; esac
fabw mission new --id "$MISSION" --name "$NAME" --ticket "GoodQA goodqa#15373" --provider claude-code >/dev/null
fabw admit --mission "$MISSION" >/dev/null
ALL=$(FABRIC_DSN="$DSN" "$FAB" job dispatch-candidates | grep -c . || true)
CANDS=$(FABRIC_DSN="$DSN" "$FAB" job dispatch-candidates --only-mission "$MISSION")
[[ "$CANDS" == "$MISSION" ]] || { fabw release --mission "$MISSION" >/dev/null 2>&1 || true; fail "dispatchable set is not exactly $MISSION: $(tr '\n' ' ' <<<"$CANDS")"; }
rec "step=4 mission=$MISSION name=$NAME gate=exactly-one dispatchable_overall=$ALL"

# --- step 5: stop the unit; one transient registry-mode fabd
mapfile -t A <<<"$SAVED_ARGV"
args=() skip=0 i=0
while (( i < ${#A[@]} )); do  # A = flock -n LOCK FAB daemon ...; keep the daemon flags
  a=${A[$i]}
  case "$a" in
    --no-dispatch|--trust-provider) ;;
    --provider) i=$((i+1));;
    --dispatch-via|--launch-limit|--only-mission) fail "the saved cmdline already has $a";;
    *) args+=("$a");;
  esac
  i=$((i+1))
done
# drop everything up to and including "daemon"
k=0; for j in "${!args[@]}"; do [[ "${args[$j]}" == daemon ]] && { k=$((j+1)); break; }; done
(( k > 0 )) || fail "no daemon subcommand in the saved cmdline"
FLAGS=$(printf ' %q' "${args[@]:$k}")
systemctl --user stop "$UNIT"
systemd-run --user --quiet --unit "$STEP_UNIT" -p Restart=no -p EnvironmentFile="$SVC/env" -- /bin/bash -c \
  "$SVC/guard.sh && exec /usr/bin/flock -n $SVC/fabd.lock $FAB daemon --dispatch-via=registry --launch-limit 1 --only-mission $MISSION --provider $BIN/fakeclaude-stdin --trust-provider$FLAGS"
rec "step=5 transient=$STEP_UNIT flags=registry,launch-limit=1,only-mission=$MISSION"

# --- step 6: poll read-only for the FINISH, then stop the transient unit
finished=0
for (( s=0; s<POLL_S; s+=5 )); do
  n=$(qr "SELECT count(*) FROM commands WHERE mission_id = '$MISSION' AND command_id LIKE 'finish:%'")
  [[ "$n" -ge 1 ]] && { finished=1; break; }
  systemctl --user is-active --quiet "$STEP_UNIT" || break
  sleep 5
done
STEP_STATE=$(systemctl --user is-failed "$STEP_UNIT" 2>/dev/null || true)
systemctl --user stop "$STEP_UNIT" 2>/dev/null || true
ATT=$(qr "SELECT id FROM attempts WHERE mission_id = '$MISSION' ORDER BY fence")
if [[ $finished == 0 && "$ATT" =~ ^r_[A-Za-z0-9_]+$ ]]; then  # timeout: stop the worker's unit; the attempt is left to lease expiry
  systemctl --user stop "fab-${ATT//_/-}.service" 2>/dev/null || true
  rec "step=6 timeout=yes worker_unit=fab-${ATT//_/-}.service attempt_left_to_lease_expiry=$ATT"
fi
NATT=$(grep -c . <<<"$ATT" || true)
STATE=$(qr "SELECT state FROM attempts WHERE mission_id = '$MISSION' ORDER BY fence DESC LIMIT 1")
OTHER=$(qr "SELECT count(*) FROM commands WHERE created_at > '$T0' AND kind IN ('START_ATTEMPT','ACK_START') AND mission_id <> '$MISSION'")
WR=$(grep -A1 -x -- '--work-root' <<<"$SAVED_ARGV" | tail -n +2 | head -1)
END=""
if [[ "$NATT" == 1 && "$ATT" =~ ^r_[a-z0-9_]+$ && -f "$WR/run/$ATT/stream.jsonl" ]]; then
  END=$(jq -c 'select(.type == "result") | .structured_output.end' "$WR/run/$ATT/stream.jsonl" | tail -1)
  STREAM_SHA=$(sha "$WR/run/$ATT/stream.jsonl")
fi
rec "step=6 finished=$finished attempts=$NATT attempt=${ATT:-none} state=$STATE end=${END:-none} other_start_ack=$OTHER step_unit=${STEP_STATE:-gone} stream_sha=${STREAM_SHA:-none}"
STEP6_OK=1
[[ $finished == 1 ]] || STEP6_OK=0
[[ "$NATT" == 1 ]] || STEP6_OK=0
[[ "$STATE" != LEASED && "$STATE" != RUNNING && "$STATE" != UNKNOWN ]] || STEP6_OK=0
[[ "$OTHER" == 0 ]] || STEP6_OK=0
[[ "$END" != *"stdin prompt missing or wrong"* && -n "$END" ]] || STEP6_OK=0
[[ "$STEP_STATE" != failed ]] || STEP6_OK=0

# --- step 7: restore by restart, then check
systemctl --user start "$UNIT"
for (( s=0; s<30; s++ )); do systemctl --user is-active --quiet "$UNIT" && break; sleep 1; done
sleep 3
R_SHA=$(sha "$UF"); R_ARGV=$(cmdline); R_STATE=$(systemctl --user is-failed "$UNIT" || true)
L0=$(LAUNCHED)
sleep "$QUIET_S"
L1=$(LAUNCHED)
RESTORE_OK=1
[[ "$R_SHA" == "$SAVED_SHA" ]] || RESTORE_OK=0
[[ "$(dropins)" == 0 ]] || RESTORE_OK=0
[[ "$R_ARGV" == "$SAVED_ARGV" ]] || RESTORE_OK=0
[[ "$R_STATE" != failed ]] || RESTORE_OK=0
[[ "$L1" == "$L0" ]] || RESTORE_OK=0
systemctl --user is-active --quiet "$UNIT" || RESTORE_OK=0
rec "step=7 unit_sha_match=$([[ $R_SHA == "$SAVED_SHA" ]] && echo yes || echo no) argv_match=$([[ "$R_ARGV" == "$SAVED_ARGV" ]] && echo yes || echo no) dropins=$(dropins) state=$R_STATE runs_launched_total=$L0..$L1 restore_ok=$RESTORE_OK"

# --- step 8: clean up the disposable mission
if fabw release --mission "$MISSION" >/dev/null 2>&1; then REL=released; else REL="refused"; fi
if [[ "$NATT" == 1 && "$ATT" =~ ^r_[a-z0-9_]+$ && -n "$WR" ]]; then
  rm -f -- "$WR/wt/$MISSION/.fakeclaude.state"
  rmdir -- "$WR/wt/$MISSION" 2>/dev/null || true
  [[ $finished == 1 ]] && rm -rf -- "${WR:?}/run/$ATT"  # an unfinished run dir stays for forensics
fi
rec "step=8 release=$REL"

[[ $RESTORE_OK == 1 ]] || fail "restore check failed: run deploy-rollback.sh and keep --no-dispatch"
[[ $STEP6_OK == 1 ]] || fail "step 6 checks failed (see step=6 record)"
rec "result=PASS"
