#!/bin/bash
# fabd-health.sh UNIT FAB_BIN        one health check (run by UNIT-health.timer)
# fabd-health.sh --clear UNIT        clear a raised alert (used by the removal path)
# fabd-health.sh --maintenance UNIT MINUTES   suppress checks for a planned stop
# (goodqa#15805)
#
# A check fails when the unit is not active, or when `fab metrics` (read-only)
# does not report fabric_build_info. After 2 consecutive failed checks it raises
# ONE critical ops-watch alert, key fabric:live-down:UNIT; the first good check
# after that emits a clear. A single blip emits nothing, and an alert already
# raised is not repeated. With the timer at 2 minutes (first run 3 minutes after
# boot), an outage that starts just after a check raises within about 4-6
# minutes. On 2026-11-12 fabd was down 15.5 hours and the only signal was a
# generic "failed user unit" line among 72 alerts.
#
# What a pass proves: the unit is active and its database answers. It does not
# prove the daemon loop is making progress (fabd exposes no heartbeat yet).
#
# Suppressed (no count, no alert): a unit that is not enabled (removed or
# deliberately disabled), and a planned stop under --maintenance until its expiry.
# A stop of an enabled unit with no maintenance window IS an outage and alerts.
#
# State: $FABD_HEALTH_STATE_DIR/UNIT.state ("fails raised"), written atomically;
# "raised" changes only after the emitter accepted the event, so a failed emit is
# retried on the next run and a raised alert survives a reboot until cleared.
#
# Environment: FABD_HEALTH_EMIT (default ~/.claude/hooks/ops_watch.py),
# FABD_HEALTH_STATE_DIR (default ~/.local/state/fabd-health), FABD_HEALTH_HOST
# (default the short hostname). FABRIC_DSN comes from fabd's EnvironmentFile via
# systemd; it is never printed, and its application_name is set to fabd-health
# so the check never looks like a second fabd to the unit's start guard.
# Exit 0 when healthy, suppressed or the alert was delivered; non-zero when an
# emit fails (also logged at crit priority, not only as a failed unit).
set -uo pipefail
EMIT=${FABD_HEALTH_EMIT:-$HOME/.claude/hooks/ops_watch.py}
STATE_DIR=${FABD_HEALTH_STATE_DIR:-$HOME/.local/state/fabd-health}
HOST=${FABD_HEALTH_HOST:-$(hostname -s)}
ACTION=check
case "${1:-}" in
  --clear) ACTION=clear; shift;;
  --maintenance) ACTION=maintenance; shift;;
esac
UNIT=${1:?usage: fabd-health.sh [--clear|--maintenance] UNIT [FAB_BIN|MINUTES]}
[[ "$UNIT" =~ ^[A-Za-z0-9._-]+$ ]] || { echo "fabd-health: bad unit name" >&2; exit 2; }
KEY="fabric:live-down:$UNIT"
mkdir -p -m 0700 "$STATE_DIR"
STATE=$STATE_DIR/$UNIT.state
MAINT=$STATE_DIR/$UNIT.maintenance
CHECKED=$STATE_DIR/$UNIT.checked
# one run at a time per unit: a timer check, --clear and --maintenance never
# interleave their state writes (PR 51 receipt F3)
exec 8> "$STATE_DIR/$UNIT.lock"
flock -w 30 8 || { echo "fabd-health: another run holds $STATE_DIR/$UNIT.lock" >&2; exit 1; }

crit() { # loud even when nobody reads failed units
  echo "fabd-health: $*" >&2
  logger -p user.crit -t fabd-health -- "$*" 2>/dev/null || true
}

emit() { # event severity message [reasserted]
  local json
  json=$(python3 -I -c 'import json,sys
d = {"unit": sys.argv[6]}
if sys.argv[7]: d["reasserted"] = True
print(json.dumps({"host":sys.argv[1],"source":"fabd-health","event":sys.argv[2],"severity":sys.argv[3],"key":sys.argv[4],"message":sys.argv[5],"detail":d}))' \
    "$HOST" "$1" "$2" "$KEY" "$3" "$UNIT" "${4:-}") || return 1
  if [[ -x "$EMIT" ]]; then
    "$EMIT" emit --json "$json" >/dev/null || { crit "emit $1 for $KEY failed"; return 1; }
  elif [[ -r "$EMIT" ]]; then
    python3 "$EMIT" emit --json "$json" >/dev/null || { crit "emit $1 for $KEY failed"; return 1; }
  else
    crit "cannot emit $1 for $KEY: emitter $EMIT not found"; return 1
  fi
}

fails=0 raised=0 since=0
if [[ -r "$STATE" ]]; then
  read -r fails raised since < "$STATE" || true
  [[ "$fails" =~ ^[0-9]+$ ]] || fails=0
  [[ "$raised" == 1 ]] || raised=0
  [[ "$since" =~ ^[0-9]+$ ]] || since=0
fi
# "since" is when the standing raise was last emitted. The ops-watch ring is
# bounded, so a raise is re-emitted every 6 h while the outage lasts (review N3).
# A failed write (ENOSPC) must never replace the state: an empty state would
# forget a standing raise and reset the count every run (review N16).
save() {
  { printf '%s %s %s\n' "$fails" "$raised" "$since" > "$STATE.tmp" && { sync "$STATE.tmp" 2>/dev/null || true; }; } \
    && mv -f "$STATE.tmp" "$STATE" \
    || { rm -f "$STATE.tmp"; crit "state write for $UNIT failed; previous state kept"; return 1; }
}
REASSERT_S=${FABD_HEALTH_REASSERT_S:-21600}

case "$ACTION" in
  clear)
    rc=0
    if [[ $raised == 1 ]]; then
      if emit clear info "Fabric $UNIT on $HOST was removed; alert cleared"; then raised=0; else rc=1; fi
    fi
    fails=0; save; exit $rc;;
  maintenance)
    mins=${2:?usage: fabd-health.sh --maintenance UNIT MINUTES}
    [[ "$mins" =~ ^[0-9]+$ ]] && (( mins >= 1 && mins <= 240 )) || { echo "fabd-health: MINUTES must be 1-240" >&2; exit 2; }
    echo $(( $(date +%s) + mins * 60 )) > "$MAINT"
    echo "fabd-health: checks of $UNIT suppressed for $mins min"; exit 0;;
esac

FAB=${2:?usage: fabd-health.sh UNIT FAB_BIN}
if [[ -r "$MAINT" ]]; then
  until_s=$(cat "$MAINT" 2>/dev/null)
  if [[ "$until_s" =~ ^[0-9]+$ ]] && (( until_s > $(date +%s) )); then
    # a window starts a fresh count, so its end does not raise on one check (review N14)
    fails=0; save
    echo "fabd-health: $UNIT in a maintenance window; check skipped"; exit 0
  fi
  rm -f "$MAINT"
fi
enabled=$(systemctl --user is-enabled "$UNIT" 2>/dev/null) || true
# an empty answer (a transient bus error) is unknown, not "disabled": skip the
# run without touching the state, so a standing raise is never cleared by it (review N19)
if [[ -z "$enabled" ]]; then
  echo "fabd-health: could not read whether $UNIT is enabled; check skipped" >&2; exit 0
fi
if [[ "$enabled" != enabled ]]; then
  echo "fabd-health: $UNIT is ${enabled:-not installed}, not enabled; check skipped"
  rc=0
  # a deliberately disabled unit must not leave a critical standing (review N8)
  if [[ $raised == 1 ]]; then
    if emit clear info "Fabric $UNIT on $HOST is ${enabled:-not installed} (not enabled); alert cleared"; then raised=0; else rc=1; fi
  fi
  fails=0; save; exit $rc
fi

reason=""
state=$(systemctl --user is-active "$UNIT" 2>/dev/null) || true
if [[ "$state" != active ]]; then
  reason="unit $UNIT is ${state:-unknown}"
else
  dsn=$(FABRIC_DSN="${FABRIC_DSN:-}" python3 -I -c '
import os, urllib.parse as u
d = os.environ["FABRIC_DSN"]
if d:
    p = u.urlsplit(d); q = dict(u.parse_qsl(p.query)); q["application_name"] = "fabd-health"; q.setdefault("connect_timeout", "10")
    d = u.urlunsplit((p.scheme, p.netloc, p.path, u.urlencode(q), ""))
print(d)') || dsn=""
  # read-only session (fab metrics also reads in a read-only transaction); the
  # output is matched, never printed, and stderr is discarded
  # a hung database must count as a failure, not hang until systemd kills the
  # check before it saves (review N4): the failure is recorded first, and the
  # probe is bounded well inside TimeoutStartSec=60
  fails=$((fails + 1)); save; fails=$((fails - 1))
  metrics=$(FABRIC_DSN="$dsn" PGOPTIONS='-c default_transaction_read_only=on' timeout 20 "$FAB" metrics 2>/dev/null) || true
  grep -q '^fabric_build_info{' <<<"$metrics" || reason="unit $UNIT is active but fab metrics has no fabric_build_info"
fi

rc=0
if [[ -z "$reason" ]]; then
  if [[ $raised == 1 ]]; then
    if emit clear info "Fabric $UNIT on $HOST is back: active and reporting metrics"; then raised=0; else rc=1; fi
  fi
  fails=0
else
  fails=$((fails + 1))
  echo "fabd-health: check failed ($fails in a row): $reason" >&2
  now=$(date +%s)
  if (( fails >= 2 )) && [[ $raised == 0 ]]; then
    if emit raise critical "Fabric $UNIT on $HOST is DOWN ($fails checks in a row, about $((fails * 2)) min): $reason"; then raised=1; since=$now; else rc=1; fi
  elif [[ $raised == 1 ]] && (( now - since >= REASSERT_S )); then
    if emit raise critical "Fabric $UNIT on $HOST is STILL DOWN ($fails checks in a row, about $((fails * 2)) min): $reason" reasserted; then since=$now; else rc=1; fi
  fi
fi
# CHECKED stamps a completed check whose state was saved (never a skip): the
# installer accepts its first check only from a stamp newer than its start
# (PR 51 receipt F1). A failed save is an error exit.
if save; then date +%s > "$CHECKED" || rc=1; else rc=1; fi
exit $rc
