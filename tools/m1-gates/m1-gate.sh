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

case "$gate" in
g0-review)
	# G0 part 1: an independent adversarial review report for the deployed
	# head ends "VERDICT: PASS" and names that head.
	#   m1-gate.sh g0-review REPORT FAB_BIN
	report=${1:-}; fab=${2:-}
	[ -f "$report" ] || die "g0-review: report $report not found"
	[ -x "$fab" ] || die "g0-review: fab binary $fab not executable"
	head=$("$fab" version 2>/dev/null | head -1 | tr -d '[:space:]')
	[ -n "$head" ] || die "g0-review: $fab version printed nothing"
	ev "deployed version $head"
	last=$(grep -E '^VERDICT: (PASS|BLOCK)$' "$report" | tail -1)
	ev "report verdict line: ${last:-none}"
	[ "$last" = "VERDICT: PASS" ] || fail "report verdict is not PASS"
	grep -q -- "$head" "$report" || fail "report does not name the deployed head $head"
	pass
	;;
g1-version)
	# G1 part 1: the deployed binary is the merged commit.
	#   m1-gate.sh g1-version FAB_BIN MERGED_SHA
	fab=${1:-}; want=${2:-}
	[ -x "$fab" ] || die "g1-version: fab binary $fab not executable"
	[[ "$want" =~ ^[0-9a-f]{12,40}$ ]] || die "g1-version: merged sha must be 12-40 hex"
	got=$("$fab" version 2>/dev/null | head -1 | tr -d '[:space:]')
	ev "deployed version ${got:-none}; merged ${want}"
	[ -n "$got" ] || fail "fab version printed nothing"
	case "$want" in "$got"*) pass ;; esac
	case "$got" in "$want"*) pass ;; esac
	fail "deployed version $got does not match merged $want"
	;;
g2 | g2b | g3 | g3-chaos | g3b | g4 | g5 | g6 | g7)
	die "$gate: NOT_IMPLEMENTED (W6 lands after W2-W5)"
	;;
*)
	die "unknown gate $gate"
	;;
esac
