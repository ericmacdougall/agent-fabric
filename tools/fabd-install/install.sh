#!/bin/bash
# fabd install on t2-dev (goodqa#15587, spec v5). Runs as the ops user on t2-dev.
#
#   MODE=rehearse FAB_BIN=/path/fab-with-no-dispatch tools/fabd-install/install.sh
#   MODE=live     FAB_BIN=/srv/fabric/bin/fab-live   tools/fabd-install/install.sh
#   MODE=remove   FAB_BIN=/srv/fabric/bin/fab-live   tools/fabd-install/install.sh   (live removal)
#
# rehearse: restores a streamed dump of fabric_live into a scratch database,
#   creates roles under the prefix fabric_svcrh (roles are cluster-wide, so a
#   rehearsal never touches fabric_svc_*), installs a unit fabd-rh.service
#   against the COPY, runs the same checks, then runs the removal path and
#   drops the copy. A trap removes everything a failed rehearsal created.
# live: the real install (prefix fabric_svc, unit fabd.service, fabric_live).
# remove: stops and disables fabd.service, restores fabric_settings.scheduler_role
#   to the value saved at install, drops the fabric_svc roles; it never touches
#   the old fabric_live_* roles or any data.
#
# Resume: a rerun with WORKDIR=<the earlier evidence directory> skips the phases
# recorded in its state file; without WORKDIR a live rerun refuses (the roles
# exist) and a rehearsal starts afresh.
# ASSUMPTION (goodqa#15587 review M7): the administrative credential is the one
# in ~/fabric-dev/test.env (FABRIC_TEST_DSN), which the deploy tooling's own
# pgcall-based scripts already use against fabric_live; the spec's "owner
# credential file the deploy scripts use" is read as that file.
#
# Never prints a DSN or a password: password files and the environment file are
# written with umask 077 into a 0700 directory, DSNs reach psql, pg_dump,
# pg_restore and fab only as environment variables of that one process.
set -euo pipefail
umask 077
export LC_ALL=C.UTF-8
export XDG_RUNTIME_DIR=${XDG_RUNTIME_DIR:-/run/user/$(id -u)}
MODE=${MODE:?MODE=rehearse|live|remove}
FAB=${FAB_BIN:?FAB_BIN}
D=$HOME/fabric-dev
[[ -x "$FAB" ]] || { echo "install: FAB_BIN is not executable" >&2; exit 2; }
# The recorded fake provider: a reviewed constant, never an environment value.
FAKE=/srv/fabric/bin/fakeclaude-live
FAKE_SHA=50d4f1d864de7b2b2ab38808d4414a3adf8a2c4b0fc0047571bdb515689d5285
HOSTLABEL=t2-dev
for v in FAB FAKE; do
  [[ "${!v}" =~ ^[A-Za-z0-9/._-]+$ ]] || { echo "install: $v has characters outside [A-Za-z0-9/._-]" >&2; exit 2; }
done
[[ "$(sha256sum "$FAKE" | cut -d' ' -f1)" == "$FAKE_SHA" ]] || { echo "install: the provider at $FAKE is not the recorded fake" >&2; exit 2; }
TS=$(date -u +%Y%m%dT%H%M%SZ)
case "$MODE" in
  rehearse) PREFIX=fabric_svcrh; UNIT=fabd-rh; SVC=$HOME/fabd-rh;;
  live|remove) PREFIX=fabric_svc; UNIT=fabd;   SVC=$HOME/fabd;;
  *) echo "install: MODE must be rehearse, live or remove" >&2; exit 2;;
esac
[[ "$SVC" =~ ^[A-Za-z0-9/._-]+$ ]] || { echo "install: SVC path has unsafe characters" >&2; exit 2; }
W=${WORKDIR:-$HOME/fabric-dev/evidence/fabd-install-$MODE-$TS}
mkdir -p -m 0700 "$W" "$SVC"
LOG=$W/install.log
exec >> "$LOG" 2>&1
STATE=$W/state
say() { echo "[$(date -u +%T)] $*"; }
die() { say "STOP: $*"; echo "STOP: $*" >&2; exit 3; }
if [[ $MODE == rehearse ]]; then
  exec 9> "$HOME/fabd-rh.lock"
  flock -n 9 || die "another rehearsal is running"
  if [[ -f "$W/db" ]]; then DB=$(cat "$W/db"); else DB=fabric_r15299_fabdrh_$(date -u +%H%M%S); echo "$DB" > "$W/db"; fi
else
  DB=fabric_live
fi
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
PGADM=$(dsn_for postgres)
TARGET=$(dsn_for "$DB")
LIVEREAD=$TARGET
ROLE_RE="^${PREFIX}_(app|runner|scheduler)$"

# --- the shared removal path (rehearse and MODE=remove)
do_remove() {
  systemctl --user stop "$UNIT.service" || true
  systemctl --user disable "$UNIT.service" || true
  rm -f "$HOME/.config/systemd/user/$UNIT.service"
  systemctl --user daemon-reload || true
  local before_old; before_old=$(pq "$PGADM" -c "SELECT count(*) FROM pg_roles WHERE rolname LIKE 'fabric\\_live\\_%'")
  if [[ -f "$SVC/old_scheduler_role" && -n "$(pq "$PGADM" -c "SELECT 1 FROM pg_database WHERE datname = '$DB'")" ]]; then
    local old; old=$(cat "$SVC/old_scheduler_role")
    [[ -z "$old" || "$old" =~ ^[a-z0-9_]+$ ]] || die "the saved scheduler_role value is not a plain role name"
    if [[ -z "$old" ]]; then
      pq "$TARGET" -c "DELETE FROM fabric_settings WHERE key = 'scheduler_role'" >/dev/null
    else
      pq "$TARGET" -c "UPDATE fabric_settings SET value = '$old' WHERE key = 'scheduler_role'" >/dev/null
    fi
    local now; now=$(pq "$TARGET" -c "SELECT COALESCE((SELECT value FROM fabric_settings WHERE key = 'scheduler_role'), '')")
    say "removal: scheduler_role restored to $now (saved value $old)"
    [[ "$now" == "$old" ]] || die "scheduler_role was not restored"
    for r in app runner scheduler; do
      [[ "${PREFIX}_$r" =~ $ROLE_RE ]] || die "refusing to drop ${PREFIX}_$r"
      if [[ -n "$(pq "$PGADM" -c "SELECT 1 FROM pg_roles WHERE rolname = '${PREFIX}_$r'")" ]]; then
        pq "$TARGET" -c "REASSIGN OWNED BY ${PREFIX}_$r TO CURRENT_USER" >/dev/null
        pq "$TARGET" -c "DROP OWNED BY ${PREFIX}_$r" >/dev/null
      fi
    done
  fi
  [[ $MODE == rehearse ]] && pq "$PGADM" -c "DROP DATABASE IF EXISTS $DB WITH (FORCE)" >/dev/null
  for r in app runner scheduler; do
    [[ "${PREFIX}_$r" =~ $ROLE_RE ]] || die "refusing to drop ${PREFIX}_$r"
    pq "$PGADM" -c "DROP ROLE IF EXISTS ${PREFIX}_$r" >/dev/null
  done
  local left after_old
  left=$(pq "$PGADM" -c "SELECT count(*) FROM pg_roles WHERE rolname LIKE '${PREFIX}\\_%'")
  after_old=$(pq "$PGADM" -c "SELECT count(*) FROM pg_roles WHERE rolname LIKE 'fabric\\_live\\_%'")
  say "removed: roles left with prefix $PREFIX = $left, old fabric_live_ roles $before_old -> $after_old"
  [[ "$left" == 0 && "$before_old" == "$after_old" ]] || die "removal left roles behind or changed the old roles"
}
if [[ $MODE == remove ]]; then
  [[ -f "$SVC/old_scheduler_role" ]] || die "no saved scheduler_role in $SVC: nothing to restore from"
  do_remove
  say "live removal complete; the dump taken at install is the restore path for data; lingering is left as it was"
  exit 0
fi
cleanup_rehearse() {
  local rc=$?
  if [[ $MODE == rehearse && "${REMOVE:-1}" == 1 && $rc != 0 ]] && ! phase_done removed; then
    say "rehearsal failed (rc $rc): removing what it created"
    do_remove || true
  fi
}
trap cleanup_rehearse EXIT

# --- rehearse: scratch copy of live data
if [[ $MODE == rehearse ]] && ! phase_done copy; then
  pq "$PGADM" -c "CREATE DATABASE $DB" >/dev/null
  PGCALL_DSN="$(dsn_for fabric_live)" python3 -I "$D/pgcall.py" pg_dump -Fc | PGCALL_DSN="$TARGET" python3 -I "$D/pgcall.py" pg_restore --exit-on-error --no-owner --no-acl -d "$DB"
  done_phase copy
fi

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
  expect_zero "live or lapsed non-terminal attempts (any host)" "SELECT count(*) FROM attempts WHERE state IN ('ADMITTED','LEASED','PREPARING','RUNNING','SUCCEEDED_UNVERIFIED','UNKNOWN','RECONCILING')"
  expect_zero "UNKNOWN attempts" "SELECT count(*) FROM attempts WHERE state = 'UNKNOWN'"
  expect_zero "HELD reservations" "SELECT count(*) FROM reservations WHERE state = 'HELD'"
  expect_zero "STARTED or requested jobs" "SELECT count(*) FROM jobs WHERE state = 'STARTED'"
  expect_zero "EXITED jobs not woken" "SELECT count(*) FROM jobs WHERE state = 'EXITED' AND NOT woken"
  expect_zero "ADMITTED missions" "SELECT count(*) FROM missions WHERE execution = 'ADMITTED'"
  expect_zero "dispatchable missions" "SELECT count(*) FROM missions m WHERE m.execution = 'ADMITTED' AND m.terminal = 'NONE' AND m.provider = 'claude-code' AND NOT EXISTS (SELECT 1 FROM waits w WHERE w.mission_id = m.id)"
  LS=$(pqr "$LIVEREAD" -c "SHOW log_statement"); say "log_statement = $LS"
  [[ "$LS" == none ]] || die "log_statement is $LS: mod and ddl log ALTER ROLE ... PASSWORD, so it must be none"
  say "log_min_error_statement = $(pqr "$LIVEREAD" -c 'SHOW log_min_error_statement') (the roles command runs with it forced to panic, see below)"
  LMD=$(pqr "$LIVEREAD" -c 'SHOW log_min_duration_statement'); say "log_min_duration_statement = $LMD"
  [[ "$LMD" == -1 ]] || die "log_min_duration_statement is $LMD: a successful ALTER ROLE would be logged with the password"
  SPL=$(pqr "$LIVEREAD" -c 'SHOW shared_preload_libraries'); say "shared_preload_libraries = $SPL"
  PSS=$(pqr "$LIVEREAD" -c "SELECT count(*) FROM pg_extension WHERE extname = 'pg_stat_statements'")
  [[ "$SPL" != *pg_stat_statements* ]] || PSS=1
  say "pg_stat_statements installed in this database: $PSS"
  if [[ "$PSS" != 0 ]]; then
    TU=$(pqr "$LIVEREAD" -c "SHOW pg_stat_statements.track_utility" 2>/dev/null || echo unknown)
    say "pg_stat_statements.track_utility = $TU"
    [[ "$TU" == off ]] || die "pg_stat_statements may store utility statements with the password text (track_utility $TU)"
  fi
  snapshot "$W/snapshot-pre.txt"
  done_phase snapshot
fi

# --- backup before any change (AC-2): live only (the rehearsal copy is disposable)
if [[ $MODE == live ]] && ! phase_done dump; then
  BK=$HOME/fabric-dev/backups; mkdir -p -m 0700 "$BK"
  DUMP=$BK/fabric_live-fabdinstall-$TS.dump
  PGCALL_DSN="$(dsn_for fabric_live)" python3 -I "$D/pgcall.py" pg_dump -Fc -f "$DUMP"
  PGCALL_DSN="$(dsn_for postgres)" python3 -I "$D/pgcall.py" pg_restore --list "$DUMP" > "$DUMP.list"
  say "dump $DUMP bytes=$(stat -c %s "$DUMP") sha256=$(sha256sum "$DUMP" | cut -c1-64) toc_lines=$(wc -l < "$DUMP.list")"
  [[ -s "$DUMP.list" ]] || die "pg_restore --list is empty"
  done_phase dump
fi
if [[ $MODE == live ]] && ! phase_done dump; then die "no verified dump"; fi

# --- roles (AC-3)
if ! phase_done roles; then
  [[ -z "$(pq "$PGADM" -c "SELECT rolname FROM pg_roles WHERE rolname LIKE '${PREFIX}\\_%'")" ]] || die "roles with the prefix $PREFIX already exist"
  OLD_SCHED=$(pqr "$LIVEREAD" -c "SELECT value FROM fabric_settings WHERE key = 'scheduler_role'" || true)
  echo "$OLD_SCHED" > "$SVC/old_scheduler_role"
  say "scheduler_role before: $OLD_SCHED"
  openssl rand -hex 24 > "$SVC/app.pw"; openssl rand -hex 24 > "$SVC/runner.pw"; chmod 600 "$SVC/app.pw" "$SVC/runner.pw"
  PGOPTIONS='-c log_min_error_statement=panic -c log_min_duration_statement=-1' FABRIC_DSN="$(dsn_for "$DB")" "$FAB" db roles --prefix "$PREFIX" --app-password-file "$SVC/app.pw" --runner-password-file "$SVC/runner.pw" 2>&1 | tail -2
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
# ExecStartPre guard of $UNIT.service: the recorded fake provider only, no second daemon
# for this database. The EnvironmentFile is inherited from systemd (not sourced here).
set -euo pipefail
[[ "\$(sha256sum $FAKE | cut -d' ' -f1)" == "$FAKE_SHA" ]] || { echo "guard: the provider at $FAKE is not the recorded fake" >&2; exit 1; }
for i in 1 2 3 4 5; do
  n=\$(PGCALL_DSN="\$FABRIC_DSN" python3 -I "$D/pgcall.py" psql -X -At -c "SELECT count(*) FROM pg_stat_activity WHERE application_name = 'fabd' AND datname = current_database() AND pid <> pg_backend_pid()")
  [[ "\$n" == 0 ]] && exit 0
  sleep 2
done
echo "guard: another session with application_name fabd exists in this database" >&2; exit 1
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
  systemd-analyze --user verify "$UNIT.service" || die "systemd-analyze verify failed for $UNIT.service"
  systemctl --user enable "$UNIT.service"
  [[ "$(systemctl --user is-enabled "$UNIT.service")" == enabled ]] || die "the unit is not enabled"
  say "unit enabled: $(systemctl --user is-enabled "$UNIT.service")"
  loginctl enable-linger "$(id -un)"
  say "linger: $(loginctl show-user "$(id -un)" -p Linger)"
  done_phase unit
fi

# --- first start and verification (AC-6)
if ! phase_done started; then
  say "starting $UNIT.service"
  if [[ $MODE == rehearse ]]; then
    FABRIC_DSN="$(dsn_for "$DB" "${PREFIX}_app" "$SVC/app.pw")" "$SVC/guard.sh" || die "the guard refuses with no daemon running"
    say "assert: the guard passes when no daemon is running (positive control)"
  fi
  STARTED_AT=$(date -u +"%Y-%m-%d %H:%M:%S")
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
  JOUT=$(journalctl --user -u "$UNIT.service" --no-pager -o cat --since "$STARTED_AT UTC") || die "journalctl failed"
  ERRN=$(printf '%s\n' "$JOUT" | grep -ciE 'error|failed|parked|STOP|lost' || true)
  say "journal lines matching error|failed|parked|STOP|lost: $ERRN"
  [[ "$ERRN" == 0 ]] || die "the journal of $UNIT.service has error-like lines"
  [[ "$c" == "$m" ]] || die "the backlog did not drain (cursor $c, max $m)"
  done_phase started
fi

# --- secret scans and assertions (AC-3 scans, AC-4, AC-5)
if ! phase_done asserts; then
  EXECSTART=$(systemctl --user show "$UNIT.service" -p ExecStart --value)
  [[ "$EXECSTART" == *"--no-dispatch"* && "$EXECSTART" != *"--no-dispatch="* ]] || die "the unit's command line lacks a plain --no-dispatch"
  [[ "$EXECSTART" == *"--provider $FAKE "* ]] || die "the unit does not use the recorded fake provider"
  say "assert: the unit command line carries --no-dispatch and the recorded fake provider"
  say "assert: argv of all processes carries no postgres:// or postgresql:// DSN: $(ps -eo args | grep -Ec '[p]ostgres(ql)?://' || true) matches"
  [[ "$(ps -eo args | grep -Ec '[p]ostgres(ql)?://' || true)" == 0 ]] || die "a DSN is visible in a process argument"
  for f in app runner; do
    # patterns come from the file (-f), never from an argument: a password in grep's own argv would leak and self-match
    if journalctl --user -u "$UNIT.service" --no-pager -o cat | grep -qF -f "$SVC/$f.pw"; then die "the $f password appears in the journal"; fi
    if ps -eo args | grep -qF -f "$SVC/$f.pw"; then die "the $f password appears in a process argument"; fi
  done
  say "assert: the journal and process arguments hold neither password"
  if [[ "$(pqr "$LIVEREAD" -c "SELECT count(*) FROM pg_extension WHERE extname = 'pg_stat_statements'")" != 0 ]]; then
    for f in app runner; do
      if pqr "$LIVEREAD" -c "SELECT query FROM pg_stat_statements" | grep -qF -f "$SVC/$f.pw"; then die "the $f password appears in pg_stat_statements"; fi
    done
    say "assert: pg_stat_statements holds neither password"
  fi
  if [[ $MODE == rehearse ]]; then
    OWN=$(dsn_for "$DB")
    FABRIC_DSN="$OWN" FABRIC_ACTOR=op:fabdinstall "$FAB" mission new --id m_rh_nd --name m_rh_nd --ticket 'GoodQA goodqa#15587' --repo fabric --branch main --provider claude-code >/dev/null
    FABRIC_DSN="$OWN" FABRIC_ACTOR=op:fabdinstall "$FAB" admit --mission m_rh_nd --key admit-m_rh_nd >/dev/null
    sleep 75
    n=$(pqr "$LIVEREAD" -c "SELECT count(*) FROM attempts WHERE mission_id = 'm_rh_nd'")
    e=$(pqr "$LIVEREAD" -c "SELECT execution FROM missions WHERE id = 'm_rh_nd'")
    say "assert: admitted mission after 75 s: attempts=$n execution=$e runs_launched_total=$(pqr "$LIVEREAD" -c "SELECT COALESCE((SELECT value FROM sched_counters WHERE name='runs_launched_total'),0)")"
    [[ "$n" == 0 && "$e" == ADMITTED ]] || die "an admitted mission was dispatched or changed"
    GDSN=$(dsn_for "$DB" "${PREFIX}_app" "$SVC/app.pw")
    if GOUT=$(FABRIC_DSN="$GDSN" "$SVC/guard.sh" 2>&1); then die "the guard allowed a second instance"; fi
    [[ "$GOUT" == *"another session with application_name fabd exists"* ]] || die "the guard refused for another reason: $GOUT"
    say "assert: the guard refuses a second instance (by its session check)"
    python3 -I - "$SVC/guard.sh" "$SVC/guard-swapped.sh" <<'PYG'
import re, sys
s = open(sys.argv[1]).read()
s = re.sub(r"== \"[0-9a-f]{64}\"", '== "' + "0" * 64 + '"', s, count=1)
open(sys.argv[2], "w").write(s)
PYG
    chmod 700 "$SVC/guard-swapped.sh"
    if GOUT=$(FABRIC_DSN="$GDSN" "$SVC/guard-swapped.sh" 2>&1); then die "the guard accepted a swapped provider"; fi
    [[ "$GOUT" == *"is not the recorded fake"* ]] || die "the swapped-provider guard refused for another reason: $GOUT"
    say "assert: the guard refuses a swapped provider (by its sha check)"
    if /usr/bin/flock -n "$SVC/fabd.lock" true 2>/dev/null; then die "the daemon does not hold its lock"; else say "assert: the daemon holds its flock"; fi
  fi
  done_phase asserts
fi

# --- removal (rehearse only; MODE=remove is the live removal)
if [[ $MODE == rehearse && "${REMOVE:-1}" == 1 ]] && ! phase_done removed; then
  do_remove
  done_phase removed
fi
say "install phases complete for MODE=$MODE; log $LOG"
