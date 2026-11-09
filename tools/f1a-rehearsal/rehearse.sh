#!/bin/bash
# F1a rehearsal on a COPY of live data (goodqa#15372, spec v4.4 AC-1(b), AC-6,
# NEG-8, NEG-10, NEG-14). Runs on t2-dev as the ops user. fabric_live is only
# read (pg_dump); every write goes to a scratch database fabric_r15299_f1a_* and
# scratch bin/backup/state directories under /var/tmp/rehearsal-f1a/.
#
#   OPS_MANIFEST_SHA256=<reviewed> NEW_SHA=<40 hex> OLD_SHA=<40 hex live commit> \
#     ~/fabric-dev/f1a-rehearse.sh
#
# What it records (the log is the evidence; no DSN or payload is printed):
#   rehearsal_commit   NEW_SHA                       rehearsal_result  PASS or FAIL
#   copy               the copy restored from a fresh fabric_live dump
#   migrate            the new binary's migrate is a no-op on the copy
#   held_scan          `fab streams held --scan` on the copy = 0 (NEG-10)
#   replaycheck        old reader vs new reader digests equal on the copy (AC-1(b))
#   deploy             deploy-live.sh in scratch mode installs NEW_SHA
#   held_after_install fab streams held --scan = 0 after the scratch install
#   rollback_checked   deploy-rollback.sh returns the scratch bin to the prior
#                      binary, the old binary ticks against the copy WITH a hold
#                      row present (NEG-6 data side), the hold row is untouched
#   rollforward        a new DEPLOY_GENERATION installs NEW_SHA again
#   held_busy_listed   held missions with a non-terminal attempt (NEG-8): listed
set -euo pipefail
umask 077
export LC_ALL=C.UTF-8
D=$HOME/fabric-dev
SHA=${NEW_SHA:?NEW_SHA}; OLD=${OLD_SHA:?OLD_SHA}; OPSM=${OPS_MANIFEST_SHA256:?OPS_MANIFEST_SHA256}
[[ "$SHA" =~ ^[0-9a-f]{40}$ && "$OLD" =~ ^[0-9a-f]{40}$ && "$OPSM" =~ ^[0-9a-f]{64}$ ]] || { echo "shas must be full hex" >&2; exit 2; }
MIG=migrations/0038_attempt_worktree.sql
VT=/var/tmp/rehearsal-f1a
mkdir -p -m 0700 "$VT"
TS=$(date -u +%Y%m%dT%H%M%SZ)
S=$VT/run-$TS; mkdir -m 0700 "$S"
LOG=$D/evidence/f1a-rehearsal-${SHA:0:7}-$TS.log
exec >> "$LOG" 2>&1   # the log is the output; read it with cat
echo "rehearsal_commit $SHA old_commit $OLD manifest $OPSM start $TS"
F=0; ok() { echo "ok: $*"; }; bad() { echo "FAIL: $*"; F=$((F+1)); }

ADMIN=$(set -a; . "$D/test.env" >/dev/null; printf '%s' "${FABRIC_TEST_DSN:-}"); [[ -n "$ADMIN" ]] || { echo "test.env sets no FABRIC_TEST_DSN" >&2; exit 2; }
dsn_for() { ADMIN="$ADMIN" python3 -I -c 'import os,sys,urllib.parse as u
p=u.urlsplit(os.environ["ADMIN"]); print(u.urlunsplit((p.scheme,p.netloc,"/"+sys.argv[1],p.query,"")))' "$1"; }
pq()  { local d=$1; shift; PGCALL_DSN="$d" python3 -I "$D/pgcall.py" psql "$@"; }
pgd() { local d=$1; shift; PGCALL_DSN="$d" python3 -I "$D/pgcall.py" pg_dump "$@"; }
pgr() { local d=$1; shift; PGCALL_DSN="$d" python3 -I "$D/pgcall.py" pg_restore "$@"; }
unset XDG_RUNTIME_DIR DBUS_SESSION_BUS_ADDRESS
PGADM=$(dsn_for postgres)
CP=fabric_r15299_f1a_$(date -u +%H%M%S)
cleanup() {
  pq "$PGADM" -X -qc "DROP DATABASE IF EXISTS $CP WITH (FORCE)" >/dev/null 2>&1 || true
  echo "cleanup: dropped copy $CP (the dump is streamed, never written); scratch sets stay under $S"
}
trap cleanup EXIT
GO=$HOME/sdk/go1.26.10/bin/go

echo "== disk"; df -h /var/tmp | tail -1
echo "== builds (the pinned build script)"
env -i HOME="$HOME" PATH=/usr/bin:/bin "$D/fab-build.sh" "$SHA" "$S/fab-new" > "$S/build-new.log" 2>&1 || { tail -5 "$S/build-new.log"; exit 3; }
env -i HOME="$HOME" PATH=/usr/bin:/bin "$D/fab-build.sh" "$OLD" "$S/fab-old" > "$S/build-old.log" 2>&1 || { tail -5 "$S/build-old.log"; exit 3; }
[[ "$("$S/fab-new" version)" == "${SHA:0:12}" && "$("$S/fab-old" version)" == "${OLD:0:12}" ]] && ok "built new ${SHA:0:12} and old ${OLD:0:12}" || bad "build versions"

echo "== copy of live data (read-only dump)"
pq "$PGADM" -X -qc "CREATE DATABASE $CP" >/dev/null
# streamed: the dump is never written to disk
pgd "$(dsn_for fabric_live)" -Fc | pgr "$(dsn_for "$CP")" --exit-on-error --no-owner --no-acl -d "$CP"
CPD=$(dsn_for "$CP")
L0=$(pq "$CPD" -X -Atqc "SELECT count(*) || '|' || max(name) FROM fabric_schema")
ok "copy $CP restored, level $L0"

echo "== migrate is a no-op, held scan is 0 (NEG-10)"
FABRIC_DSN="$CPD" "$S/fab-new" migrate >/dev/null 2>&1
L1=$(pq "$CPD" -X -Atqc "SELECT count(*) || '|' || max(name) FROM fabric_schema")
[[ "$L0" == "$L1" && "$L1" == *"$MIG" ]] && ok "migrate: level unchanged ($L1)" || bad "level $L0 -> $L1"
SCAN=$(FABRIC_DSN="$CPD" "$S/fab-new" streams held --scan | tail -1)
[[ "$SCAN" == "held streams: 0" ]] && ok "held_scan: $SCAN" || bad "held_scan: $SCAN"

echo "== replaycheck: old reader vs new reader on the copy (AC-1(b))"
rc_build() { # commit outdir : replaycheck source from NEW tree built inside the given commit's tree
  local c=$1 o=$2 t=$S/src-$1
  git clone -q --no-checkout "$HOME/repos/fabric" "$t"
  git -C "$t" checkout -q --detach "$c"
  [[ "$(git -C "$t" rev-parse HEAD)" == "$c" ]]
  mkdir -p "$t/tools/replaycheck"
  git -C "$HOME/repos/fabric" show "$SHA:tools/replaycheck/main.go" > "$t/tools/replaycheck/main.go"
  # built inside the clone, so the build info carries the commit (vcs.revision)
  (cd "$t" && GOFLAGS=-mod=readonly CGO_ENABLED=0 GOTOOLCHAIN=local GOWORK=off GOENV=off PATH="$HOME/sdk/go1.26.10/bin:$PATH" go build -buildvcs=true -o "$o" ./tools/replaycheck)
}
rc_build "$OLD" "$S/rc-old"; rc_build "$SHA" "$S/rc-new"
FABRIC_DSN="$CPD" "$S/rc-old" > "$S/rc-old.tsv" 2>"$S/rc-old.err" || true
FABRIC_DSN="$CPD" "$S/rc-new" > "$S/rc-new.tsv" 2>"$S/rc-new.err" || true
echo "missions: old $(grep -vc '^#\|^mission' "$S/rc-old.tsv") new $(grep -vc '^#\|^mission' "$S/rc-new.tsv")"
git -C "$HOME/repos/fabric" show "$SHA:tools/replaycheck/diff.py" > "$S/diff.py"
python3 -I "$S/diff.py" "$S/rc-old.tsv" "$S/rc-new.tsv" && ok "replaycheck: every mission matches" || bad "replaycheck differs (see $S/rc-*.tsv)"

echo "== scratch deploy, rollback and roll-forward (deploy-live.sh and deploy-rollback.sh in scratch mode)"
B=$S/set; mkdir -m 0700 "$B"; mkdir -m 0755 "$B/bin"; mkdir -m 0700 "$B/backups" "$B/deploys"
for f in /srv/fabric/bin/*; do [[ -f "$f" && ! -L "$f" ]] && cp -p -- "$f" "$B/bin/"; done
PRIOR=$(sha256sum "$B/bin/fab-live" | cut -d' ' -f1)
echo "prior scratch fab-live: $("$B/bin/fab-live" version) sha256 ${PRIOR:0:12}"
dl() { # generation
  env -i HOME="$HOME" PATH=/usr/bin:/bin BIN_DIR="$B/bin" TARGET_DB="$CP" BACKUP_DIR="$B/backups" STATE_DIR="$B/deploys" \
    OPS_MANIFEST_SHA256="$OPSM" DEPLOY_GENERATION="$1" "$D/deploy-live.sh" "$SHA" "$MIG"
}
dl 1 > "$S/deploy1.log" 2>&1 && ok "deploy: installed ($(tail -1 "$S/deploy1.log" | cut -c1-120))" || { bad "deploy failed"; tail -5 "$S/deploy1.log"; }
[[ "$("$B/bin/fab-live" version)" == "${SHA:0:12}" ]] && ok "scratch fab-live is the candidate" || bad "scratch fab-live is $("$B/bin/fab-live" version)"
HA=$(FABRIC_DSN="$CPD" "$B/bin/fab-live" streams held --scan | tail -1)
[[ "$HA" == "held streams: 0" ]] && ok "held_after_install: $HA" || bad "held_after_install: $HA"

STATE=$(ls "$B/deploys"/*.state | head -1); STATE=$(basename "$STATE")
REV12=""; for f in "$B/backups"/fab-live-*.bin; do [[ "$(sha256sum "$f" | cut -d' ' -f1)" == "$PRIOR" ]] && REV12=$(basename "$f" .bin) && REV12=${REV12#fab-live-}; done
[[ -n "$REV12" ]] && ok "prior backup rev $REV12" || bad "no backup matches the prior sha"
# A hold row on a healthy mission, then the rollback (NEG-6 data side, NEG-8).
HM=$(pq "$CPD" -X -Atqc "SELECT id FROM missions ORDER BY id LIMIT 1")
pq "$CPD" -X -qc "INSERT INTO outbox_cursors (consumer, last_id) VALUES ('sched.hold.$HM', COALESCE((SELECT last_id FROM outbox_cursors WHERE consumer = 'sched'), 0))" >/dev/null
echo "held_busy_listed: $(pq "$CPD" -X -Atqc "SELECT count(*) FROM attempts a JOIN outbox_cursors c ON c.consumer = 'sched.hold.' || a.mission_id WHERE a.state IN ('LEASED','PREPARING','RUNNING','UNKNOWN','RECONCILING','SUCCEEDED_UNVERIFIED')") held missions with a non-terminal attempt"
env -i HOME="$HOME" PATH=/usr/bin:/bin BIN_DIR="$B/bin" TARGET_DB="$CP" BACKUP_DIR="$B/backups" STATE_DIR="$B/deploys" \
  OPS_MANIFEST_SHA256="$OPSM" ROLLBACK_TICKET=goodqa-15372 "$D/deploy-rollback.sh" "$REV12" "$PRIOR" "$STATE" > "$S/rollback.log" 2>&1 \
  && ok "rollback ran ($(tail -1 "$S/rollback.log" | cut -c1-100))" || { bad "rollback failed"; tail -5 "$S/rollback.log"; }
[[ "$(sha256sum "$B/bin/fab-live" | cut -d' ' -f1)" == "$PRIOR" ]] && ok "rollback_checked: scratch fab-live is the prior binary again ($("$B/bin/fab-live" version))" || bad "rollback_checked: not the prior binary"
[[ "$(pq "$CPD" -X -Atqc "SELECT count(*) FROM outbox_cursors WHERE consumer = 'sched.hold.$HM'")" == 1 ]] && ok "the hold row is present for the old binary" || bad "hold row missing"
# the old binary ticks: its read paths work against data with the hold row (metrics, status).
FABRIC_DSN="$CPD" "$B/bin/fab-live" status > "$S/old-status.txt" 2>&1 && ok "the prior binary reads the copy with the hold row (status rc 0)" || { bad "the prior binary status failed"; head -3 "$S/old-status.txt"; }
FABRIC_DSN="$CPD" "$B/bin/fab-live" metrics > "$S/old-metrics.txt" 2>&1 && ok "the prior binary renders metrics against the copy" || { bad "prior binary metrics failed"; head -3 "$S/old-metrics.txt"; }

dl 2 > "$S/deploy2.log" 2>&1 && ok "rollforward: generation 2 installed ($(tail -1 "$S/deploy2.log" | cut -c1-100))" || { bad "rollforward failed"; tail -5 "$S/deploy2.log"; }
[[ "$("$B/bin/fab-live" version)" == "${SHA:0:12}" ]] && ok "rollforward: scratch fab-live is the candidate again" || bad "rollforward: $("$B/bin/fab-live" version)"
pq "$CPD" -X -qc "DELETE FROM outbox_cursors WHERE consumer = 'sched.hold.$HM'" >/dev/null
echo "held after the hold row is removed: $(FABRIC_DSN="$CPD" "$B/bin/fab-live" streams held --scan | tail -1)"
if [[ $F == 0 ]]; then echo "rehearsal_result PASS rehearsal_commit $SHA"; else echo "rehearsal_result FAIL ($F failed) rehearsal_commit $SHA"; exit 1; fi
