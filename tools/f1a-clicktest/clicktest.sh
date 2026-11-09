#!/bin/bash
# F1a click-test (goodqa#15372, spec v4.3 AC-7 and NEG-12). Run by a tester who
# is not the author, on the DEPLOYED build, against a disposable database.
#
#   FAB_BIN=/srv/fabric/bin/fab-live ADMIN_DSN_FILE=/path/admin.dsn \
#   FAKE_PROVIDER=/path/tools/fakeclaude/fakeclaude.sh tools/f1a-clicktest/clicktest.sh
#
# ADMIN_DSN_FILE is a file (mode 0600) holding an admin DSN of the Postgres
# server to create the disposable database on; the script never prints it and
# never puts a password on a command line or in the environment of a child
# other than as a DSN variable. It creates ONLY a database fabtest_N and roles
# fabtest_N_scheduler, fabtest_N_app and fabtest_N_runner; it refuses to run if
# any of them exists, refuses the default role prefix, never touches a fabric_*
# role or fabric_live, and its exit trap drops the database and the roles and
# then proves nothing named fabtest_ remains. The role passwords exist only in
# shell variables and reach fab through process substitution: no password file
# is ever written. The work directory (mode 0700) holds only the daemon's
# work root; it is left in place and its literal path is printed at the end,
# for the tester to remove with an explicit command.
#
# What it shows (the output is the evidence; no payload text is printed):
#   1  a healthy mission and two poisoned missions (an unknown event kind, and
#      a payload of the wrong type) are seeded; the daemon starts on them
#   2  `fab streams held` lists exactly the two poisoned missions, `fab status`
#      and the gauge say 2, the healthy mission is not held, the daemon is still
#      alive after 60 seconds
#   3  the daemon is stopped and restarted: the same two holds, nothing double
#      counted
#   4  the poisoned rows are repaired by ONE owner transaction that lifts the
#      append-only trigger for that statement only; within two ticks the holds
#      are gone, `fab streams held --scan` reports 0 and the gauge is 0
# The presence of the held rows is checked in SQL as well as in the CLI output.
set -euo pipefail
umask 077
export LC_ALL=C.UTF-8

die() { echo "clicktest: STOP: $*" >&2; exit 2; }
FAB=${FAB_BIN:?set FAB_BIN to the deployed fab binary}
FAKE=${FAKE_PROVIDER:?set FAKE_PROVIDER to the recorded fake provider}
ADMIN_FILE=${ADMIN_DSN_FILE:?set ADMIN_DSN_FILE to a file holding the admin DSN}
[[ -x "$FAB" && -x "$FAKE" ]] || die "FAB_BIN and FAKE_PROVIDER must be executable"
[[ -f "$ADMIN_FILE" && "$(stat -c %a "$ADMIN_FILE")" == 600 ]] || die "ADMIN_DSN_FILE must be a mode 0600 file"
for v in $(compgen -e); do case "$v" in PG*|FABRIC_*) die "inherited $v is set";; esac; done

N=$(( ($(date +%s) * 7 + $$) % 900000 + 100000 ))
DB=fabtest_$N
PREFIX=fabtest_$N
[[ "$DB" =~ ^fabtest_[0-9]+$ && "$PREFIX" != fabric ]] || die "refusing database or prefix $DB"
WORK=$(mktemp -d "${TMPDIR:-/var/tmp}/f1a-clicktest.XXXXXX")
chmod 0700 "$WORK"
mkdir -m 0700 "$WORK/wr"

ADMIN=$(cat "$ADMIN_FILE")
dsn_for() {  # database [user passwordvar]
  ADMIN="$ADMIN" PW="${3:+${!3}}" python3 -I -c '
import os, sys, urllib.parse as u
p = u.urlsplit(os.environ["ADMIN"]); db = sys.argv[1]
netloc = p.netloc
if len(sys.argv) > 2:
    host = p.netloc.rsplit("@", 1)[-1]
    pw = os.environ["PW"]
    netloc = u.quote(sys.argv[2]) + ":" + u.quote(pw) + "@" + host
print(u.urlunsplit((p.scheme, netloc, "/" + db, p.query, "")))' "$@"
}
# psql gets its connection from PG* environment variables of its own process
# (readable only by this user), never a DSN with a password on its command
# line (/proc/PID/cmdline is world-readable on a shared host).
pgenv() {  # database: shell exports for libpq
  ADMIN="$ADMIN" python3 -I -c '
import os, shlex, sys, urllib.parse as u
p = u.urlsplit(os.environ["ADMIN"])
q = dict(u.parse_qsl(p.query))
env = {"PGHOST": p.hostname or "", "PGPORT": str(p.port or ""), "PGUSER": u.unquote(p.username or ""),
       "PGPASSWORD": u.unquote(p.password or ""), "PGDATABASE": sys.argv[1]}
if "sslmode" in q:
    env["PGSSLMODE"] = q["sslmode"]
for k, v in env.items():
    if v:
        print("export %s=%s" % (k, shlex.quote(v)))' "$1"
}
adminq() ( eval "$(pgenv postgres)"; export PGAPPNAME=clicktest; exec psql -X -q -At -v ON_ERROR_STOP=1 "$@" )
ownerq() ( eval "$(pgenv "$DB")"; exec psql -X -q -At -v ON_ERROR_STOP=1 "$@" )

DAEMON_PID=""
cleanup() {
  set +e
  [[ -n "$DAEMON_PID" ]] && wait "$DAEMON_PID" 2>/dev/null   # the daemon is bounded by timeout(1) and ends by itself
  adminq -c "DROP DATABASE IF EXISTS $DB WITH (FORCE)" >/dev/null 2>&1
  for r in scheduler app runner; do adminq -c "DROP ROLE IF EXISTS ${PREFIX}_$r" >/dev/null 2>&1; done
  left=$(adminq -c "SELECT (SELECT count(*) FROM pg_database WHERE datname LIKE 'fabtest\_%') + (SELECT count(*) FROM pg_roles WHERE rolname LIKE 'fabtest\_%')" 2>/dev/null)
  echo "teardown: fabtest_ databases and roles remaining: ${left:-unknown}; no password file was written; the work directory (the daemon's work root only) is $WORK"
}
trap cleanup EXIT

[[ "$(adminq -c "SELECT count(*) FROM pg_database WHERE datname = '$DB'")" == 0 ]] || die "$DB already exists"
[[ "$(adminq -c "SELECT count(*) FROM pg_roles WHERE rolname LIKE '${PREFIX}\\_%'")" == 0 ]] || die "a role ${PREFIX}_* already exists"
echo "clicktest: database $DB, role prefix $PREFIX, fab $("$FAB" version | head -1)"

PW_APP=$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')
PW_RUNNER=$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')
adminq -c "CREATE DATABASE $DB"
export FABRIC_DSN; FABRIC_DSN=$(dsn_for "$DB")
"$FAB" migrate
"$FAB" db roles --prefix "$PREFIX" --app-password-file <(printf '%s' "$PW_APP") --runner-password-file <(printf '%s' "$PW_RUNNER") >/dev/null
APP_DSN=$(dsn_for "$DB" "${PREFIX}_app" PW_APP)
RUNNER_DSN=$(dsn_for "$DB" "${PREFIX}_runner" PW_RUNNER)

# 1: seed one healthy and two poisoned missions (the owner plants the rows, as a writer would).
export FABRIC_ACTOR=op:clicktest
for m in m_ct_healthy m_ct_unknown m_ct_badtype; do
  "$FAB" mission new --id "$m" --name "$m" --ticket 'GoodQA goodqa#15372' --repo fabric --branch main --provider claude-code >/dev/null
  "$FAB" admit --mission "$m" --key "admit-$m" >/dev/null
done
plant() {  # mission seq kind payload
  ownerq -c "BEGIN; INSERT INTO events (mission_id, seq, kind, actor, command_id, payload) VALUES ('$1', $2, '$3', 'clicktest', 'plant-$1', '$4'::jsonb); INSERT INTO outbox (mission_id, seq, kind, payload) VALUES ('$1', $2, '$3', '$4'::jsonb); COMMIT" >/dev/null
}
plant m_ct_unknown 100 NEW_FUTURE_KIND '{"kind":"NEW_FUTURE_KIND","mission":"m_ct_unknown","seq":100}'
plant m_ct_badtype 100 ATTEMPT_STARTED '{"kind":"ATTEMPT_STARTED","mission":"m_ct_badtype","seq":100,"fence":"not-a-number"}'

# The daemon runs under timeout(1) for a fixed time and ends by itself: the
# script never signals a process.
start_daemon() {  # seconds it may run
  printf '{"rungs":{"R0":{"effort":"n/a","models":{"claude":"fake-model"}}}}\n' > "$WORK/tiers.json"
  FABRIC_DSN="$APP_DSN" FABRIC_RUNNER_DSN="$RUNNER_DSN" timeout "$1" "$FAB" daemon --provider "$FAKE" --trust-provider \
    --work-root "$WORK/wr" --tier-table "$WORK/tiers.json" --rung R0 --host h_fabtest >>"${DAEMON_LOG:-/dev/null}" 2>&1 &
  DAEMON_PID=$!
}
held_sql() { ownerq -c "SELECT count(*) FROM outbox_cursors WHERE consumer LIKE 'sched.hold.%'"; }
metric()   { FABRIC_DSN="$APP_DSN" "$FAB" metrics | awk '$1 == "fabric_held_streams" {print $2}'; }
wait_for() {  # description command expected seconds
  for _ in $(seq 1 "$4"); do [[ "$(eval "$2")" == "$3" ]] && { echo "ok: $1"; return 0; }; sleep 1; done
  echo "FAIL: $1 (last value: $(eval "$2"))"; return 1
}

start_daemon 130
wait_for "two holds appear in the database" held_sql 2 30
export FABRIC_DSN="$APP_DSN"
held1=$("$FAB" streams held)
echo "--- fab streams held"; echo "$held1"
grep -q '^m_ct_unknown	NEW_FUTURE_KIND	unknown-kind' <<<"$held1" && echo "ok: the unknown-kind mission is listed with its class"
grep -q '^m_ct_badtype	ATTEMPT_STARTED	undecodable' <<<"$held1" && echo "ok: the type-mismatch mission is listed with its class"
! grep -q m_ct_healthy <<<"$held1" && echo "ok: the healthy mission is not held"
grep -q '^held streams: 2$' <<<"$held1" && echo "ok: held streams: 2"
echo "--- fab status"; "$FAB" status | sed -n '1,3p'
[[ "$(metric)" == 2 ]] && echo "ok: fabric_held_streams 2"
sleep 60
[[ -d "/proc/$DAEMON_PID" ]] && echo "ok: the daemon is alive after 60 seconds"
echo "--- the healthy mission is not left at ADMITTED"; "$FAB" status | grep m_ct_healthy | cut -c1-120

# 3: restart: the first daemon's time is up; a second one starts on the same database.
wait "$DAEMON_PID" 2>/dev/null || true; DAEMON_PID=""
start_daemon 100
sleep 15
[[ "$(held_sql)" == 2 && "$(metric)" == 2 ]] && echo "ok: after a restart the same two holds, counted once"
[[ -d "/proc/$DAEMON_PID" ]] && echo "ok: the restarted daemon is alive"

# 4: repair the two rows in ONE owner transaction, trigger lifted for that statement only.
ownerq <<SQL >/dev/null
BEGIN;
ALTER TABLE events DISABLE TRIGGER events_append_only;
ALTER TABLE outbox DISABLE TRIGGER USER;
UPDATE events SET kind = 'INTENT_REVISED', payload = '{"kind":"INTENT_REVISED","mission":"m_ct_unknown","seq":100}'::jsonb WHERE mission_id = 'm_ct_unknown' AND seq = 100;
UPDATE outbox SET kind = 'INTENT_REVISED', payload = '{"kind":"INTENT_REVISED","mission":"m_ct_unknown","seq":100}'::jsonb WHERE mission_id = 'm_ct_unknown' AND seq = 100;
DELETE FROM outbox WHERE mission_id = 'm_ct_badtype' AND seq = 100;
DELETE FROM events WHERE mission_id = 'm_ct_badtype' AND seq = 100;
ALTER TABLE events ENABLE TRIGGER events_append_only;
ALTER TABLE outbox ENABLE TRIGGER USER;
COMMIT;
SQL
wait_for "both holds are released within a few ticks" held_sql 0 20
[[ "$("$FAB" streams held --scan | tail -1)" == "held streams: 0" ]] && echo "ok: fab streams held --scan reports 0"
[[ "$(metric)" == 0 ]] && echo "ok: fabric_held_streams 0"

# 5 (optional, NEG-6 mixed binaries): OLD_FAB_BIN is the binary built BEFORE
# F1a (the previous live one). A hold row exists on the healthy mission; the
# old binary has no hold check and must keep ticking against it, the data it
# leaves must still be read by the new binary, and the new binary then
# releases the hold.
if [[ -n "${OLD_FAB_BIN:-}" ]]; then
  [[ -x "$OLD_FAB_BIN" ]] || die "OLD_FAB_BIN must be executable"
  wait "$DAEMON_PID" 2>/dev/null || true; DAEMON_PID=""
  ownerq -c "INSERT INTO outbox_cursors (consumer, last_id) VALUES ('sched.hold.m_ct_healthy', COALESCE((SELECT last_id FROM outbox_cursors WHERE consumer = 'sched'), 0))"
  [[ "$(held_sql)" == 1 ]] && echo "ok: a hold row exists on the healthy mission"
  OLD_VER=$("$OLD_FAB_BIN" version | head -1)
  printf '{"rungs":{"R0":{"effort":"n/a","models":{"claude":"fake-model"}}}}\n' > "$WORK/tiers.json"
  FABRIC_DSN="$APP_DSN" FABRIC_RUNNER_DSN="$RUNNER_DSN" timeout 40 "$OLD_FAB_BIN" daemon --provider "$FAKE" --trust-provider \
    --work-root "$WORK/wr" --tier-table "$WORK/tiers.json" --rung R0 --host h_fabtest >>"${DAEMON_LOG:-/dev/null}" 2>&1 &
  DAEMON_PID=$!
  sleep 25
  [[ -d "/proc/$DAEMON_PID" ]] && echo "ok: the old binary ($OLD_VER) is still ticking after 25 seconds with a hold row present"
  wait "$DAEMON_PID" 2>/dev/null || true; DAEMON_PID=""
  [[ "$(held_sql)" == 1 ]] && echo "ok: the old binary left the hold row alone"
  start_daemon 60
  wait_for "the new binary releases the hold the old binary left" held_sql 0 30
  [[ "$("$FAB" streams held --scan | tail -1)" == "held streams: 0" ]] && echo "ok: nothing held after the mixed run"
fi
echo "clicktest: done"
