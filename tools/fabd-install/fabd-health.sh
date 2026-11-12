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

crit() { # loud even when nobody reads failed units
  echo "fabd-health: $*" >&2
  logger -p user.crit -t fabd-health -- "$*" 2>/dev/null || true
}

emit() { # event severity message
  local json
  json=$(python3 -I -c 'import json,sys; print(json.dumps({"host":sys.argv[1],"source":"fabd-health","event":sys.argv[2],"severity":sys.argv[3],"key":sys.argv[4],"message":sys.argv[5],"detail":{"unit":sys.argv[6]}}))' \
    "$HOST" "$1" "$2" "$KEY" "$3" "$UNIT") || return 1
  if [[ -x "$EMIT" ]]; then
    "$EMIT" emit --json "$json" >/dev/null || { crit "emit $1 for $KEY failed"; return 1; }
  elif [[ -r "$EMIT" ]]; then
    python3 "$EMIT" emit --json "$json" >/dev/null || { crit "emit $1 for $KEY failed"; return 1; }
  else
    crit "cannot emit $1 for $KEY: emitter $EMIT not found"; return 1
  fi
}

fails=0 raised=0
if [[ -r "$STATE" ]]; then
  read -r fails raised < "$STATE" || true
  [[ "$fails" =~ ^[0-9]+$ ]] || fails=0
  [[ "$raised" == 1 ]] || raised=0
fi
save() { printf '%s %s\n' "$fails" "$raised" > "$STATE.tmp" && mv -f "$STATE.tmp" "$STATE"; }

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
    echo "fabd-health: $UNIT in a maintenance window; check skipped"; exit 0
  fi
  rm -f "$MAINT"
fi
enabled=$(systemctl --user is-enabled "$UNIT" 2>/dev/null) || true
if [[ "$enabled" != enabled ]]; then
  echo "fabd-health: $UNIT is ${enabled:-not installed}, not enabled; check skipped"
  fails=0; save; exit 0
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
    p = u.urlsplit(d); q = dict(u.parse_qsl(p.query)); q["application_name"] = "fabd-health"
    d = u.urlunsplit((p.scheme, p.netloc, p.path, u.urlencode(q), ""))
print(d)') || dsn=""
  # read-only session (fab metrics also reads in a read-only transaction); the
  # output is matched, never printed, and stderr is discarded
  metrics=$(FABRIC_DSN="$dsn" PGOPTIONS='-c default_transaction_read_only=on' "$FAB" metrics 2>/dev/null) || true
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
  if (( fails >= 2 )) && [[ $raised == 0 ]]; then
    if emit raise critical "Fabric $UNIT on $HOST is DOWN ($fails checks in a row, about $((fails * 2)) min): $reason"; then raised=1; else rc=1; fi
  fi
fi
save
exit $rc
