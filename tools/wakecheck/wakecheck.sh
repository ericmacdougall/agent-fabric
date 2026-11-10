#!/bin/bash
# goodqa#15586 F19a-0 wake check on a scratch database: does a committed command
# wake a running fabd within 100 ms? It creates ONLY a database fabtest_N and
# roles fabtest_N_*, starts a daemon on them under timeout(1), runs the
# wakecheck binary (10 accepted commands, one per idle second) and reports the
# latency from commit to the scheduler consuming the command, plus the durable
# wake_hints_total counter before and after (`fab metrics`). It never touches
# fabric_live or any fabric_svc_* role, and its exit trap drops the database and
# the roles. Credentials: ADMIN_DSN_FILE (a mode 0600 file) is read inside this
# script; a DSN with a password is never put on a command line.
#
# If the script is killed with SIGKILL nothing can clean up: drop the leftovers by hand
# (DROP DATABASE fabtest_N WITH (FORCE); DROP ROLE fabtest_N_scheduler, fabtest_N_app,
# fabtest_N_runner); the database comment names the pid and start time of the run.
#
#   FAB_BIN=/srv/fabric/bin/fab-live WAKECHECK_BIN=<built tools/wakecheck> \
#   FAKE_PROVIDER=/srv/fabric/bin/fakeclaude-live ADMIN_DSN_FILE=$HOME/ct-dry/admin.dsn \
#     tools/wakecheck/wakecheck.sh
set +x   # never trace: the admin DSN passes through this script
set -euo pipefail
umask 077
export LC_ALL=C.UTF-8

die() { echo "wakecheck.sh: STOP: $*" >&2; exit 2; }
FAB=${FAB_BIN:?set FAB_BIN to the fab binary under test}
WC=${WAKECHECK_BIN:?set WAKECHECK_BIN to the built tools/wakecheck}
FAKE=${FAKE_PROVIDER:?set FAKE_PROVIDER to the recorded fake provider}
ADMIN_FILE=${ADMIN_DSN_FILE:?set ADMIN_DSN_FILE to a file holding the admin DSN}
[[ -x "$FAB" && -x "$WC" && -x "$FAKE" ]] || die "FAB_BIN, WAKECHECK_BIN and FAKE_PROVIDER must be executable"
[[ -f "$ADMIN_FILE" && "$(stat -c %a "$ADMIN_FILE")" == 600 ]] || die "ADMIN_DSN_FILE must be a mode 0600 file"
for v in $(compgen -e); do case "$v" in PG*|FABRIC_*|BASH_ENV|SHELLOPS|BASH_XTRACEFD|ENV) die "inherited $v is set";; esac; done
N=$(( $(od -An -N4 -tu4 /dev/urandom | tr -d ' ') % 900000 + 100000 ))   # random: two runs rarely collide; cleanup drops only what this run created
DB=fabtest_$N
PREFIX=fabtest_$N
[[ "$DB" =~ ^fabtest_[0-9]+$ ]] || die "refusing database $DB"
WORK=$(mktemp -d "${TMPDIR:-/var/tmp}/wakecheck.XXXXXX")
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
pgenv() {  # database: shell exports for libpq (never a DSN on a command line)
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
adminq() ( eval "$(pgenv postgres)"; export PGAPPNAME=wakecheck; exec psql -X -q -At -v ON_ERROR_STOP=1 "$@" )

DAEMON_PID=""; CREATED_DB=0; CREATED_ROLES=0   # cleanup drops only what this run itself created
cleanup() {
  set +e
  # No signal is sent: the daemon runs under timeout -k 5 45 and ends by itself (at most 50 s).
  # DROP DATABASE WITH (FORCE) cuts its connections at once, so the teardown does not wait for it.
  [[ -n "$DAEMON_PID" ]] && echo "teardown: dropping now; the daemon (pid $DAEMON_PID) ends on its own clock within 50 s"
  [[ "$CREATED_DB" == 1 ]] && adminq -c "DROP DATABASE IF EXISTS $DB WITH (FORCE)" >/dev/null 2>&1
  [[ "$CREATED_ROLES" == 1 ]] && for try in 1 2 3 4 5; do   # the daemon's last connections can still be closing
    for r in scheduler app runner; do adminq -c "DROP ROLE IF EXISTS ${PREFIX}_$r" >/dev/null 2>"$WORK/droprole.err"; done
    [[ "$(adminq -c "SELECT count(*) FROM pg_roles WHERE rolname LIKE '${PREFIX}\\_%'" 2>/dev/null)" == 0 ]] && break
    sleep 2
  done
  [[ -s "$WORK/droprole.err" ]] && echo "teardown: last DROP ROLE error: $(head -c 300 "$WORK/droprole.err")"
  left=$(adminq -c "SELECT (SELECT count(*) FROM pg_database WHERE datname LIKE 'fabtest\_%') + (SELECT count(*) FROM pg_roles WHERE rolname LIKE 'fabtest\_%')" 2>/dev/null)
  echo "teardown: fabtest_ databases and roles remaining: ${left:-unknown}; work directory $WORK"
}
trap cleanup EXIT

[[ "$(adminq -c "SELECT count(*) FROM pg_database WHERE datname = '$DB'")" == 0 ]] || die "$DB already exists"
[[ "$(adminq -c "SELECT count(*) FROM pg_roles WHERE rolname LIKE '${PREFIX}\\_%'")" == 0 ]] || die "a role ${PREFIX}_* already exists"
echo "wakecheck.sh: database $DB, fab $("$FAB" version | head -1)"

PW_APP=$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')
PW_RUNNER=$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')
adminq -c "CREATE DATABASE $DB"
CREATED_DB=1
adminq -c "COMMENT ON DATABASE $DB IS 'wakecheck.sh pid $$ started $(date -u +%FT%TZ)'" >/dev/null
export FABRIC_DSN; FABRIC_DSN=$(dsn_for "$DB")
"$FAB" migrate
CREATED_ROLES=1   # set before the call: a partial failure may still have created a role
"$FAB" db roles --prefix "$PREFIX" --app-password-file <(printf '%s' "$PW_APP") --runner-password-file <(printf '%s' "$PW_RUNNER") >/dev/null
APP_DSN=$(dsn_for "$DB" "${PREFIX}_app" PW_APP)
RUNNER_DSN=$(dsn_for "$DB" "${PREFIX}_runner" PW_RUNNER)

printf '{"rungs":{"R0":{"effort":"n/a","models":{"claude":"fake-model"}}}}\n' > "$WORK/tiers.json"
FABRIC_DSN="$APP_DSN" FABRIC_RUNNER_DSN="$RUNNER_DSN" timeout -k 5 45 "$FAB" daemon --provider "$FAKE" --trust-provider \
  --work-root "$WORK/wr" --tier-table "$WORK/tiers.json" --rung R0 --host h_fabtest >"$WORK/daemon.log" 2>&1 &
DAEMON_PID=$!
sleep 3
[[ -d "/proc/$DAEMON_PID" ]] || { cat "$WORK/daemon.log"; die "the daemon did not start"; }

counter() { FABRIC_DSN="$APP_DSN" "$FAB" metrics | awk '$1 ~ /^fabric_sched_counter_total.*wake_hints_total/ {print $2}' | head -1; }
before=$(counter); before=${before:-0}
echo "wake_hints_total before: $before"
FABRIC_DSN="$APP_DSN" "$WC"
after=$(counter); after=${after:-0}
echo "wake_hints_total after: $after"
if [[ "$after" =~ ^[0-9]+$ && "$before" =~ ^[0-9]+$ && "$after" -gt "$before" ]]; then
  echo "ok: the wake_hints_total counter rose by $((after - before)) while the commands ran"
else
  echo "NOTE: wake_hints_total did not rise (before $before, after $after): the binary under test has no counter, or the daemon was not woken"
fi
echo "wakecheck.sh: DONE"
