#!/bin/sh
# Schema-dump check: migration 0022 gives the same schema whether it is applied in order
# (fresh database) or after 0023/0024 (a database that already ran them).
# Run on t2-dev. Needs ~/fabric-s1 (branch feat/native-slice1) and ~/repos/fabric (0023, 0024 sources).
set -eu
. ~/fabric-dev/test.env
ADMIN=$FABRIC_TEST_DSN
W=$(mktemp -d)
cd ~/fabric-s1/internal/store/migrations
mkdir "$W/m"
cp 00*.sql "$W/m/"
[ -f "$W/m/0021_effect_settle_guard.sql" ] || git -C ~/repos/fabric show main:internal/store/migrations/0021_effect_settle_guard.sql > "$W/m/0021_effect_settle_guard.sql"
[ -f "$W/m/0023_effect_dispatch_deadlines.sql" ] || git -C ~/repos/fabric show main:internal/store/migrations/0023_effect_dispatch_deadlines.sql > "$W/m/0023_effect_dispatch_deadlines.sql"
ls "$W/m"/0024_* >/dev/null 2>&1 || git -C ~/repos/fabric show feat/ac12-publish:internal/store/migrations/0024_publish_quarantine.sql > "$W/m/0024_publish_quarantine.sql"
ls "$W/m" | tr '\n' ' '; echo
dsn() { echo "$ADMIN" | sed "s#/[A-Za-z_0-9]*\(?\|\$\)#/$1\1#"; }
mk() { psql "$ADMIN" -qAtc "DROP DATABASE IF EXISTS $1 WITH (FORCE)" -c "CREATE DATABASE $1" >/dev/null; }
apply() { db=$1; shift; for f in "$@"; do psql "$(dsn $db)" -q -v ON_ERROR_STOP=1 -1 -f "$W/m/$f" >/dev/null || { echo "FAILED $db $f"; exit 1; }; done; }
mk commute_a; mk commute_b
ALL=$(ls "$W/m" | sort)
apply commute_a $ALL
NO22=$(ls "$W/m" | sort | grep -v '^0022_')
apply commute_b $NO22 0022_agent_turns.sql
for d in a b; do pg_dump -s --no-owner --no-privileges "$(dsn commute_$d)" | grep -v '^--' | grep -v '^$' | grep -v 'restrict' > "$W/$d.sql"; done
if diff -u "$W/a.sql" "$W/b.sql" > "$W/diff.txt"; then echo "IDENTICAL schema ($(wc -l < "$W/a.sql") lines) fresh-order vs 0022-after-0023/0024"; else echo "DIFFERENT"; head -60 "$W/diff.txt"; fi
psql "$ADMIN" -qAtc "DROP DATABASE commute_a WITH (FORCE)" -c "DROP DATABASE commute_b WITH (FORCE)" >/dev/null
