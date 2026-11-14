#!/bin/bash
# goodqa#15966 (Fabric M1): machine-checkable gates G0-G7 (gates r3, spec-m1-v3).
#
#   tools/m1-gates/m1-gate.sh <gate> [args]
#
# Exit 0: the gate holds. Exit 1: the gate fails. Exit 2: usage or environment
# error, which is never a pass. Every gate prints evidence lines prefixed
# "evidence:" and never prints a secret value (secret-bearing checks print
# counts only).
#
# A non-author runs each gate against the deployed commit on the Fabric node.
# Gates not yet implemented exit 2 (NOT_IMPLEMENTED), so an unfinished gate
# can never read as a pass.
set -uo pipefail

die() { echo "m1-gate: $*" >&2; exit 2; }
pass() { echo "PASS $gate"; exit 0; }
fail() { echo "FAIL $gate: $*"; exit 1; }
ev() { echo "evidence: $*"; }

gate=${1:-}
[ -n "$gate" ] || die "usage: m1-gate.sh g0-review|g1-version|g2|g2b|g3|g3-chaos|g3b|g4|g5|g6|g7 [args]"
shift

# deployed_version FAB_BIN FULL_SHA: the binary's version command must exit 0
# and print 12-40 lowercase hex that is a prefix of the full 40-hex commit.
deployed_version() {
	local fab=$1 want=$2 out rc
	out=$("$fab" version 2>/dev/null); rc=$?
	out=$(printf '%s' "$out" | head -1 | tr -d '[:space:]')
	ev "version command exit $rc, output '${out:0:40}'; expected commit $want"
	[ "$rc" = 0 ] || fail "version command exited $rc"
	[[ "$out" =~ ^[0-9a-f]{12,40}$ ]] || fail "version output is not 12-40 hex"
	case "$want" in "$out"*) ;; *) fail "deployed version $out is not a prefix of $want" ;; esac
}

case "$gate" in
g0-review)
	# G0 part 1: an independent adversarial review report bound to the
	# deployed head. The report must carry exactly one "Reviewed HEAD:" field
	# equal to the full deployed commit, and its last non-empty line must be
	# exactly "VERDICT: PASS". The deployed binary must report that commit.
	#   m1-gate.sh g0-review REPORT FAB_BIN DEPLOYED_SHA40
	report=${1:-}; fab=${2:-}; want=${3:-}
	[ -f "$report" ] || die "g0-review: report $report not found"
	[ -x "$fab" ] || die "g0-review: fab binary $fab not executable"
	[[ "$want" =~ ^[0-9a-f]{40}$ ]] || die "g0-review: deployed sha must be 40 hex"
	deployed_version "$fab" "$want"
	heads=$(grep -E '^Reviewed HEAD:' "$report") || true
	n=$(printf '%s' "$heads" | grep -c . || true)
	ev "Reviewed HEAD fields: $n"
	[ "$n" = 1 ] || fail "report must have exactly one Reviewed HEAD field, found $n"
	got=$(printf '%s' "$heads" | sed -E 's/^Reviewed HEAD:[[:space:]]*//; s/[[:space:]]+$//')
	ev "Reviewed HEAD $got"
	[ "$got" = "$want" ] || fail "Reviewed HEAD is not the deployed commit"
	last=$(grep -v '^[[:space:]]*$' "$report" | tail -1 | sed -E 's/[[:space:]]+$//')
	ev "last line: ${last:0:60}"
	[ "$last" = "VERDICT: PASS" ] || fail "last non-empty line is not VERDICT: PASS"
	pass
	;;
g1-version)
	# G1 part 1: the deployed binary is the integrated commit.
	#   m1-gate.sh g1-version FAB_BIN TARGET_SHA40
	fab=${1:-}; want=${2:-}
	[ -x "$fab" ] || die "g1-version: fab binary $fab not executable"
	[[ "$want" =~ ^[0-9a-f]{40}$ ]] || die "g1-version: target sha must be the full 40 hex"
	deployed_version "$fab" "$want"
	pass
	;;
g2 | g2b | g3 | g3-chaos | g3b | g4 | g5 | g6 | g7)
	die "$gate: NOT_IMPLEMENTED (W6 lands after W2-W5)"
	;;
*)
	die "unknown gate $gate"
	;;
esac
