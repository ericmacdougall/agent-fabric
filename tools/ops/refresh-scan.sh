#!/bin/bash
# refresh-scan.sh   (goodqa#15361; the documented trivy DB refresh and control-scan repair)
#
# deploy-live.sh refuses when the Trivy DB's own UpdatedAt is more than 24 h old.
# Only a newer published DB fixes that, and a new DB needs a new control scan
# (its PAIR.ok carries the DB digest), which changes one line of the pinned ops
# manifest and so its sha256 (OPS_MANIFEST_SHA256). This script does exactly that
# and nothing else:
#   1  check the current manifest verifies (every pinned file matches)
#   2  move the old control-scan-2 directory and log aside (kept as evidence)
#   3  download the newest published DB into the pinned Trivy cache
#   4  run the control scan on the recorded control binary with the recorded
#      --expect-id values (CVE-2026-33815, CVE-2026-33816, CVE-2025-68121, as
#      printed by the previous CONTROL OK line); PAIR.ok is written by the
#      checker only on CONTROL OK
#   5  write a new manifest: identical to the old one except the PAIR.ok line,
#      refusing if any other pinned file differs from its recorded hash
#   6  print the new manifest sha256 for review
# On any failure before step 5 the old control-scan directory is restored.
# It never edits deploy-live.sh, the age window or any other pin. The DB is
# not pinned by the manifest; the pairing is the control. The new manifest hash
# is NOT usable until a reviewer records it.
set -euo pipefail
umask 077
D=$HOME/fabric-dev
TS=$(date -u +%Y%m%dT%H%M%SZ)
EV=$D/evidence/t15299
MAN=$D/t15299-ops.sha256
T=$D/tools/trivy-0.76.1/trivy
export TRIVY_CACHE_DIR=$D/tools/trivy-cache
BIN=$EV/control-bin/fab-live-150bac7
PAIRDIR=$EV/control-scan-2
PAIRFILE=$PAIRDIR/PAIR.ok
LOG=$EV/refresh-scan-$TS.log
[[ $# == 0 ]] || { echo "usage: refresh-scan.sh (no arguments; it only re-pairs the control scan)"; exit 2; }
exec > >(tee -a "$LOG") 2>&1
echo "refresh-scan $TS"

[[ "$(sha256sum -- "$T" | cut -d' ' -f1)" == d1dfca6ae10a1ad4540bb94e64f0510077c47008920a9b554986b39dca573d83 ]] || { echo "refresh-scan: trivy binary is not the pinned one"; exit 2; }
echo "== 1 current manifest"
EXCL=(-e "  $PAIRFILE")
grep -vF "${EXCL[@]}" "$MAN" > "$MAN.chk-$TS"
(cd / && sha256sum --strict --quiet -c "$MAN.chk-$TS") || { echo "refresh-scan: the current manifest does not verify (apart from the PAIR.ok line); refusing"; rm -f "$MAN.chk-$TS"; exit 2; }
rm -f "$MAN.chk-$TS"
echo "manifest ok: $(sha256sum < "$MAN" | cut -d' ' -f1)"
grep -qF -- "  $PAIRFILE" "$MAN" || { echo "refresh-scan: manifest does not pin $PAIRFILE"; exit 2; }
[[ -d "$PAIRDIR" && -f "$BIN" ]] || { echo "refresh-scan: control scan directory or control binary missing"; exit 2; }
[[ "$(sha256sum -- "$BIN" | cut -d" " -f1)" == "a925b88a1e61beb72a8b7d2ef6bb1532ca7faba94fcfa612bd43d452249c27df" ]] || { echo "refresh-scan: the control binary is not the recorded one"; exit 2; }
echo "refresh-scan script sha256 $(sha256sum -- "$0" | cut -d" " -f1)"

echo "== 2 move the old control scan aside"
OLD=$PAIRDIR.pre-$TS
ok=0
moved=0
restore() { if [[ $ok != 1 && $moved == 1 ]]; then rm -rf -- "$PAIRDIR"; mv -- "$OLD" "$PAIRDIR"; echo "refresh-scan: FAILED; old control scan restored (the new DB stays in the cache, so the gate refuses until a refresh succeeds)"; fi; }
trap restore EXIT
mv -- "$PAIRDIR" "$OLD"
moved=1
[[ -f "$EV/control-scan-2.log" ]] && cp -- "$EV/control-scan-2.log" "$OLD/control-scan-2.log"

echo "== 3 download the newest published DB"
# trivy skips the download while the cached DB's NextUpdate is in the future, so
# a fresh-looking cache would be re-used and nothing would change. Remove only the
# cached vulnerability DB (same pinned binary, same cache dir, no other cache
# kind, no registry flag) so the download below always fetches the newest one.
(cd / && env -i HOME="$HOME" PATH=/usr/bin:/bin LC_ALL=C.UTF-8 TRIVY_CACHE_DIR="$TRIVY_CACHE_DIR" "$T" clean --vuln-db) 2>&1 | tail -3
(cd / && env -i HOME="$HOME" PATH=/usr/bin:/bin LC_ALL=C.UTF-8 TRIVY_CACHE_DIR="$TRIVY_CACHE_DIR" "$T" image --download-db-only --no-progress) 2>&1 | tail -3
(cd / && env -i HOME="$HOME" PATH=/usr/bin:/bin LC_ALL=C.UTF-8 TRIVY_CACHE_DIR="$TRIVY_CACHE_DIR" "$T" version --format json) | python3 -I -c 'import json,sys; v=json.load(sys.stdin).get("VulnerabilityDB",{}); print("db version", v.get("Version"), "updated", v.get("UpdatedAt"), "next", v.get("NextUpdate"))'

echo "== 4 control scan"
set +e
"$D/fab-scan.sh" "$BIN" "$PAIRDIR" self --expect-id CVE-2026-33815 --expect-id CVE-2026-33816 --expect-id CVE-2025-68121 > "$EV/control-scan-2.log" 2>&1
rc=$?
set -e
tail -4 "$EV/control-scan-2.log" | cut -c1-240
[[ $rc == 0 && -f "$PAIRFILE" ]] || { echo "refresh-scan: control scan rc=$rc (no CONTROL OK)"; exit 3; }
grep -q "CONTROL OK" "$PAIRDIR/CHECK.txt" || { echo "refresh-scan: CONTROL OK missing from CHECK.txt"; exit 3; }

echo "== 5 new manifest"
NEW=$MAN.new-$TS
OLDHASH=$(sha256sum -- "$OLD/PAIR.ok" | cut -d' ' -f1)
NEWHASH=$(sha256sum -- "$PAIRFILE" | cut -d' ' -f1)
python3 -I - "$MAN" "$NEW" "$PAIRFILE" "$OLDHASH" "$NEWHASH" <<'PY'
import sys
man, new, pair, oldh, newh = sys.argv[1:6]
lines = open(man).read().split("\n")
out, changed = [], 0
for l in lines:
    if l.endswith("  " + pair):
        assert l.startswith(oldh + "  "), "the PAIR.ok line is not the previous control scan's"
        out.append(newh + "  " + pair); changed += 1
    else:
        out.append(l)
assert changed == 1, "expected exactly the PAIR.ok line"
open(new, "w").write("\n".join(out))
PY
# every other pinned file must still match: verify the old manifest minus the PAIR.ok line
grep -vF "${EXCL[@]}" "$MAN" > "$NEW.rest"
(cd / && sha256sum --strict --quiet -c "$NEW.rest") || { echo "refresh-scan: another pinned file differs from its recorded hash; refusing"; rm -f "$NEW" "$NEW.rest"; exit 4; }
rm -f "$NEW.rest"
echo "manifest diff (old vs new):"
diff <(cat "$MAN") <(cat "$NEW") | cut -c1-200 || true
[[ "$(diff <(cat "$MAN") <(cat "$NEW") | grep -c '^[<>]')" == 2 ]] || { echo "refresh-scan: the manifest diff is not the PAIR.ok line"; rm -f "$NEW"; exit 4; }
cp -- "$MAN" "$MAN.pre-$TS"
mv -- "$NEW" "$MAN"
ok=1
echo "== 6 result"
(cd / && sha256sum --strict --quiet -c "$MAN") && echo "new manifest verifies"
echo "OLD_MANIFEST_SHA256 $(sha256sum < "$MAN.pre-$TS" | cut -d' ' -f1)"
echo "NEW_MANIFEST_SHA256 $(sha256sum < "$MAN" | cut -d' ' -f1)"
echo "log $LOG ; old control scan kept at $OLD ; old manifest kept at $MAN.pre-$TS"
