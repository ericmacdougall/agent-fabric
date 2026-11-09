#!/bin/bash
# fabd install on t2-dev (goodqa#15587, spec v3). Runs as the ops user on t2-dev.
#
#   MODE=rehearse FAB_BIN=/path/fab-with-no-dispatch tools/fabd-install/install.sh
#   MODE=live     FAB_BIN=/srv/fabric/bin/fab-live   tools/fabd-install/install.sh
#
# rehearse: restores a streamed dump of fabric_live into a scratch database,
#   creates roles under the prefix fabric_svcrh (roles are cluster-wide, so a
#   rehearsal never touches fabric_svc_*), installs a unit fabd-rh.service
#   against the COPY, runs the same checks, then removes everything it created.
# live: the real install (prefix fabric_svc, unit fabd.service, fabric_live).
#
# Never prints a DSN or a password: password files and the environment file are
# written with umask 077 into a 0700 directory, DSNs reach psql and fab only as
# environment variables of that one process.
# Stops on the first failed check (set -e plus explicit stop rules); a rerun
# resumes after the last recorded phase in $STATE.
set -euo pipefail
umask 077
export LC_ALL=C.UTF-8
MODE=${MODE:?MODE=rehearse|live}
FAB=${FAB_BIN:?FAB_BIN}
D=$HOME/fabric-dev
[[ -x "$FAB" ]] || { echo "install: FAB_BIN is not executable" >&2; exit 2; }
case "$MODE" in
  rehearse) PREFIX=fabric_svcrh; UNIT=fabd-rh; DB=fabric_r15299_fabdrh_$(date -u +%H%M%S); SVC=$HOME/fabd-rh;;
  live)     PREFIX=fabric_svc;   UNIT=fabd;    DB=fabric_live;                                 SVC=$HOME/fabd;;
  *) echo "install: MODE must be rehearse or live" >&2; exit 2;;
esac
FAKE=/srv/fabric/bin/fakeclaude-live
FAKE_SHA=${FAKE_SHA:-$(sha256sum "$FAKE" | cut -d' ' -f1)}   # pinned below in the unit's guard
HOSTLABEL=${HOST_LABEL:-t2-dev}
TS=$(date -u +%Y%m%dT%H%M%SZ)
W=${WORKDIR:-$HOME/fabric-dev/evidence/fabd-install-$MODE-$TS}
mkdir -p -m 0700 "$W" "$SVC"
LOG=$W/install.log
exec >> "$LOG" 2>&1
STATE=$W/state
say() { echo "[$(date -u +%T)] $*"; }
say "fabd install MODE=$MODE prefix=$PREFIX db=$DB unit=$UNIT fab=$("$FAB" version)"

ADMIN=$(set -a; . "$D/test.env" >/dev/null; printf '%s' "${FABRIC_TEST_DSN:-}")
[[ -n "$ADMIN" ]] || { echo "install: test.env sets no FABRIC_TEST_DSN" >&2; exit 2; }
dsn_for() {  # db [user pwfile]
  ADMIN="$ADMIN" PWF="${3:-}" python3 -I -c '
import os, sys, urllib.parse as u
p = u.urlsplit(os.environ["ADMIN"]); db = sys.argv[1]; netloc = p.netloc
if len(sys.argv) > 2:
    pw = open(os.environ["PWF"]).read().strip()
    netloc = u.quote(sys.argv[2], safe="") + ":" + u.quote(pw, safe="") + "@" + p.netloc.rsplit("@", 1)[-1]
q = dict(u.parse_qsl(p.query)); q["application_name"] = "fabd" if len(sys.argv) > 2 else "fabd-install"
print(u.urlunsplit((p.scheme, netloc, "/" + db, u.urlencode(q), "")))' "$@"
}
pq()  { local d=$1; shift; PGOPTIONS="${PQ_OPTS:-}" PGCALL_DSN="$d" python3 -I "$D/pgcall.py" psql -X -At -v ON_ERROR_STOP=1 "$@"; }
pqr() { PQ_OPTS='-c default_transaction_read_only=on' pq "$@"; }
done_phase() { echo "$1" >> "$STATE"; say "phase $1 done"; }
phase_done() { [[ -f "$STATE" ]] && grep -qx "$1" "$STATE"; }
die() { say "STOP: $*"; echo "STOP: $*" >&2; exit 3; }

PGADM=$(dsn_for postgres)
TARGET=$(dsn_for "$DB")

# --- rehearse: scratch copy of live data
if [[ $MODE == rehearse ]] && ! phase_done copy; then
  pq "$PGADM" -c "CREATE DATABASE $DB" >/dev/null
  pgcall() { local d=$1; shift; PGCALL_DSN="$d" python3 -I "$D/pgcall.py" "$@"; }
  pgcall "$(dsn_for fabric_live)" pg_dump -Fc | pgcall "$TARGET" pg_restore --exit-on-error --no-owner --no-acl -d "$DB"
  done_phase copy
fi
LIVEREAD=$(dsn_for "$DB")

# --- snapshot and stop rule (AC-1)
snapshot() {  # outfile
  {
    echo "snapshot $(date -u +%FT%TZ) db=$DB"
    echo "== cursor";        pqr "$LIVEREAD" -c "SELECT COALESCE((SELECT last_id FROM outbox_cursors WHERE consumer='sched'),-1)"
    echo "== max outbox id"; pqr "$LIVEREAD" -c "SELECT COALESCE(max(id),0) FROM outbox"
    echo "== counters";      pqr "$LIVEREAD" -c "SELECT name, value FROM sched_counters ORDER BY name"
    echo "== waits";         pqr "$LIVEREAD" -c "SELECT mission_id, kind, key FROM waits ORDER BY 1"
    echo "== missions";      pqr "$LIVEREAD" -c "SELECT id, execution, delivery, terminal FROM missions ORDER BY 1"
    echo "== attempts";      pqr "$LIVEREAD" -c "SELECT id, mission_id, state FROM attempts ORDER BY 1"
    echo "== jobs";          pqr "$LIVEREAD" -c "SELECT id, mission_id, state, woken FROM jobs ORDER BY 1"
    echo "== reservations by attempt state and state"; pqr "$LIVEREAD" -c "SELECT a.state, r.state, count(*) FROM reservations r JOIN attempts a ON a.id = r.attempt_id GROUP BY 1,2 ORDER BY 1,2"
    echo "== fabric_settings"; pqr "$LIVEREAD" -c "SELECT key, value FROM fabric_settings ORDER BY 1"
    echo "== held streams"; FABRIC_DSN="$LIVEREAD" "$FAB" streams held --scan | tail -1
    echo "== runs_launched_total"; pqr "$LIVEREAD" -c "SELECT COALESCE((SELECT value FROM sched_counters WHERE name='runs_launched_total'),0)"
  } > "$1"
}
expect_zero() {  # description sql
  local n; n=$(pqr "$LIVEREAD" -c "$2")
  say "precondition $1 = $n"
  [[ "$n" == 0 ]] || die "precondition failed: $1 is $n, expected 0"
}
if ! phase_done snapshot; then
  expect_zero "lapsed or live non-terminal attempts (any host)" "SELECT count(*) FROM attempts WHERE state IN ('ADMITTED','LEASED','PREPARING','RUNNING','SUCCEEDED_UNVERIFIED','UNKNOWN','RECONCILING')"
  expect_zero "UNKNOWN attempts" "SELECT count(*) FROM attempts WHERE state = 'UNKNOWN'"
  expect_zero "HELD reservations" "SELECT count(*) FROM reservations WHERE state = 'HELD'"
  expect_zero "STARTED or requested jobs" "SELECT count(*) FROM jobs WHERE state = 'STARTED'"
  expect_zero "EXITED jobs not woken" "SELECT count(*) FROM jobs WHERE state = 'EXITED' AND NOT woken"
  expect_zero "ADMITTED missions" "SELECT count(*) FROM missions WHERE execution = 'ADMITTED'"
  expect_zero "dispatchable missions" "SELECT count(*) FROM missions m WHERE m.execution = 'ADMITTED' AND m.terminal = 'NONE' AND m.provider = 'claude-code' AND NOT EXISTS (SELECT 1 FROM waits w WHERE w.mission_id = m.id)"
  LS=$(pqr "$LIVEREAD" -c "SHOW log_statement"); say "log_statement = $LS"
  [[ "$LS" == none || "$LS" == mod ]] || die "log_statement is $LS: a role password in DDL would reach the server log"
  say "log_min_error_statement = $(pqr "$LIVEREAD" -c 'SHOW log_min_error_statement')"
  snapshot "$W/snapshot-pre.txt"
  done_phase snapshot
fi

# --- backup before any change (AC-2): live only (the rehearsal copy is itself disposable)
if [[ $MODE == live ]] && ! phase_done dump; then
  BK=$HOME/fabric-dev/backups; mkdir -p -m 0700 "$BK"
  DUMP=$BK/fabric_live-fabdinstall-$TS.dump
  PGCALL_DSN="$(dsn_for fabric_live)" python3 -I "$D/pgcall.py" pg_dump -Fc -f "$DUMP"
  PGCALL_DSN="$(dsn_for postgres)" python3 -I "$D/pgcall.py" pg_restore --list "$DUMP" > "$DUMP.list"
  say "dump $DUMP bytes=$(stat -c %s "$DUMP") sha256=$(sha256sum "$DUMP" | cut -c1-64) toc_lines=$(wc -l < "$DUMP.list")"
  [[ -s "$DUMP.list" ]] || die "pg_restore --list is empty"
  done_phase dump
fi
[[ $MODE == live ]] && ! phase_done dump && die "no verified dump"

# --- roles (AC-3)
if ! phase_done roles; then
  [[ -z "$(pq "$PGADM" -c "SELECT rolname FROM pg_roles WHERE rolname LIKE '${PREFIX}\\_%'")" ]] || die "roles with the prefix $PREFIX already exist"
  OLD_SCHED=$(pqr "$LIVEREAD" -c "SELECT value FROM fabric_settings WHERE key = 'scheduler_role'" || true)
  echo "$OLD_SCHED" > "$SVC/old_scheduler_role"
  say "scheduler_role before: $OLD_SCHED"
  openssl rand -hex 24 > "$SVC/app.pw"; openssl rand -hex 24 > "$SVC/runner.pw"; chmod 600 "$SVC/app.pw" "$SVC/runner.pw"
  FABRIC_DSN="$(dsn_for "$DB")" "$FAB" db roles --prefix "$PREFIX" --app-password-file "$SVC/app.pw" --runner-password-file "$SVC/runner.pw" 2>&1 | tail -2
  APPD=$(dsn_for "$DB" "${PREFIX}_app" "$SVC/app.pw"); RUND=$(dsn_for "$DB" "${PREFIX}_runner" "$SVC/runner.pw")
  FABRIC_DSN="$APPD" FABRIC_RUNNER_DSN="$RUND" "$FAB" db check-runner && say "check-runner ok"
  say "scheduler_role after: $(pqr "$LIVEREAD" -c "SELECT value FROM fabric_settings WHERE key = 'scheduler_role'")"
  say "app can write fabric_schema: $(pqr "$LIVEREAD" -c "SELECT has_table_privilege('${PREFIX}_app','fabric_schema','INSERT')")"
  say "runner INSERT on jobs: $(pqr "$LIVEREAD" -c "SELECT has_table_privilege('${PREFIX}_runner','jobs','INSERT')")"
  done_phase roles
fi

# --- environment file, guard, unit (AC-4, AC-5, AC-7)
if ! phase_done unit; then
  APPD=$(dsn_for "$DB" "${PREFIX}_app" "$SVC/app.pw"); RUND=$(dsn_for "$DB" "${PREFIX}_runner" "$SVC/runner.pw")
  printf 'FABRIC_DSN=%s\nFABRIC_RUNNER_DSN=%s\nFABRIC_HOME=%s\n' "$APPD" "$RUND" "$SVC/home" > "$SVC/env"
  chmod 600 "$SVC/env"; mkdir -p -m 0700 "$SVC/home" "$SVC/work"
  printf '{"rungs":{"R0":{"effort":"n/a","models":{"claude":"fake-model"}}}}\n' > "$SVC/tiers.json"
  cat > "$SVC/guard.sh" <<GUARD
#!/bin/bash
# ExecStartPre guard of $UNIT.service: the recorded fake provider only, no second daemon.
set -euo pipefail
[[ "\$(sha256sum $FAKE | cut -d' ' -f1)" == "$FAKE_SHA" ]] || { echo "guard: the provider at $FAKE is not the recorded fake" >&2; exit 1; }
set -a; . "$SVC/env"; set +a
for i in 1 2 3 4 5; do
  n=\$(PGCALL_DSN="\$FABRIC_DSN" python3 -I "$D/pgcall.py" psql -X -At -c "SELECT count(*) FROM pg_stat_activity WHERE application_name = 'fabd' AND pid <> pg_backend_pid()")
  [[ "\$n" == 0 ]] && exit 0
  sleep 2
done
echo "guard: another session with application_name fabd exists" >&2; exit 1
GUARD
  chmod 700 "$SVC/guard.sh"
  mkdir -p "$HOME/.config/systemd/user"
  cat > "$HOME/.config/systemd/user/$UNIT.service" <<UNITF
[Unit]
Description=Fabric fabd ($MODE, no dispatch, recorded fake provider only; goodqa#15587)
StartLimitIntervalSec=300
StartLimitBurst=5

[Service]
EnvironmentFile=$SVC/env
ExecStartPre=$SVC/guard.sh
ExecStart=/usr/bin/flock -n $SVC/fabd.lock $FAB daemon --no-dispatch --provider $FAKE --trust-provider --work-root $SVC/work --tier-table $SVC/tiers.json --rung R0 --host $HOSTLABEL
Restart=on-failure
RestartSec=5
KillSignal=SIGTERM
TimeoutStopSec=15

[Install]
WantedBy=default.target
UNITF
  systemctl --user daemon-reload
  systemd-analyze --user verify "$UNIT.service" 2>&1 | head -5 || true
  loginctl enable-linger "$(id -un)"
  say "linger: $(loginctl show-user "$(id -un)" -p Linger)"
  done_phase unit
fi

# --- first start and verification (AC-6)
if ! phase_done started; then
  say "starting $UNIT.service"
  systemctl --user start "$UNIT.service"
  for i in $(seq 1 40); do
    c=$(pqr "$LIVEREAD" -c "SELECT COALESCE((SELECT last_id FROM outbox_cursors WHERE consumer='sched'),-1)")
    m=$(pqr "$LIVEREAD" -c "SELECT COALESCE(max(id),0) FROM outbox")
    [[ "$c" == "$m" ]] && break
    sleep 3
  done
  say "cursor $c max outbox id $m; unit state: $(systemctl --user is-active "$UNIT.service")"
  snapshot "$W/snapshot-post.txt"
  diff "$W/snapshot-pre.txt" "$W/snapshot-post.txt" > "$W/snapshot.diff" || true
  say "snapshot diff lines: $(wc -l < "$W/snapshot.diff")"
  cut -c1-160 "$W/snapshot.diff" | head -40
  say "held streams after: $(FABRIC_DSN="$LIVEREAD" "$FAB" streams held --scan | tail -1)"
  say "journal errors since start: $(journalctl --user -u "$UNIT.service" --since '-10min' --no-pager -p err 2>/dev/null | grep -vc '^-- ' || true)"
  say "postgres log or password scan: the password files are $(stat -c %a "$SVC/app.pw") and the env file $(stat -c %a "$SVC/env")"
  [[ "$c" == "$m" ]] || die "the backlog did not drain (cursor $c, max $m)"
  done_phase started
fi
# --- assertions (AC-4, AC-5): the unit's command line, nothing dispatched, the guards refuse
if ! phase_done asserts; then
  export XDG_RUNTIME_DIR=${XDG_RUNTIME_DIR:-/run/user/$(id -u)}
  EXECSTART=$(systemctl --user show "$UNIT.service" -p ExecStart --value)
  [[ "$EXECSTART" == *"--no-dispatch"* ]] || die "the unit's command line has no --no-dispatch"
  say "assert: the unit command line carries --no-dispatch and the fake provider $( [[ "$EXECSTART" == *"$FAKE"* ]] && echo yes || echo NO )"
  [[ "$EXECSTART" == *"$FAKE"* ]] || die "the unit does not use the recorded fake provider"
  PID=$(systemctl --user show "$UNIT.service" -p MainPID --value)
  say "assert: argv of the daemon processes carries no postgres:// DSN: $(ps -eo args | grep -c '[p]ostgres://' || true) matches"
  if [[ $MODE == rehearse ]]; then
    # an admitted claude-code mission must stay at zero attempts across more than one backoff cycle
    OWN=$(dsn_for "$DB")
    FABRIC_DSN="$OWN" FABRIC_ACTOR=op:fabdinstall "$FAB" mission new --id m_rh_nd --name m_rh_nd --ticket 'GoodQA goodqa#15587' --repo fabric --branch main --provider claude-code >/dev/null
    FABRIC_DSN="$OWN" FABRIC_ACTOR=op:fabdinstall "$FAB" admit --mission m_rh_nd --key admit-m_rh_nd >/dev/null
    sleep 75
    n=$(pqr "$LIVEREAD" -c "SELECT count(*) FROM attempts WHERE mission_id = 'm_rh_nd'")
    e=$(pqr "$LIVEREAD" -c "SELECT execution FROM missions WHERE id = 'm_rh_nd'")
    say "assert: admitted mission after 75 s: attempts=$n execution=$e runs_launched_total=$(pqr "$LIVEREAD" -c "SELECT COALESCE((SELECT value FROM sched_counters WHERE name='runs_launched_total'),0)")"
    [[ "$n" == 0 && "$e" == ADMITTED ]] || die "an admitted mission was dispatched or changed"
    # guards: a second instance and a swapped provider are refused
    if "$SVC/guard.sh" 2>/dev/null; then die "the guard allowed a second instance"; else say "assert: the guard refuses a second instance"; fi
    python3 -I - "$SVC/guard.sh" "$SVC/guard-swapped.sh" <<'PYG'
import re, sys
s = open(sys.argv[1]).read()
s = re.sub(r"== \"[0-9a-f]{64}\"", '== "' + "0" * 64 + '"', s, count=1)
open(sys.argv[2], "w").write(s)
PYG
    chmod 700 "$SVC/guard-swapped.sh"
    if "$SVC/guard-swapped.sh" 2>/dev/null; then die "the guard accepted a swapped provider"; else say "assert: the guard refuses a swapped provider"; fi
    # a manual start while the unit runs is refused by the lock
    if /usr/bin/flock -n "$SVC/fabd.lock" true 2>/dev/null; then die "the daemon does not hold its lock"; else say "assert: the daemon holds its flock"; fi
  fi
  done_phase asserts
fi

# --- removal (rehearse only; the live removal is a documented runbook step)
if [[ $MODE == rehearse && "${REMOVE:-1}" == 1 ]] && ! phase_done removed; then
  export XDG_RUNTIME_DIR=${XDG_RUNTIME_DIR:-/run/user/$(id -u)}
  systemctl --user stop "$UNIT.service" || true
  systemctl --user disable "$UNIT.service" || true
  rm -f "$HOME/.config/systemd/user/$UNIT.service"
  systemctl --user daemon-reload || true
  pq "$PGADM" -c "DROP DATABASE IF EXISTS $DB WITH (FORCE)" >/dev/null
  for r in app runner scheduler; do pq "$PGADM" -c "DROP ROLE IF EXISTS ${PREFIX}_$r" >/dev/null || true; done
  say "removed: roles left with prefix $PREFIX = $(pq "$PGADM" -c "SELECT count(*) FROM pg_roles WHERE rolname LIKE '${PREFIX}\\_%'"), databases left = $(pq "$PGADM" -c "SELECT count(*) FROM pg_database WHERE datname = '$DB'"), old roles still present = $(pq "$PGADM" -c "SELECT count(*) FROM pg_roles WHERE rolname LIKE 'fabric\\_live\\_%'")"
  done_phase removed
fi
say "install phases complete for MODE=$MODE; log $LOG"
