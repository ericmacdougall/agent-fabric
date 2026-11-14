#!/bin/bash
# goodqa#15966 AC-26: every implemented gate must pass its pass fixture and
# fail (exit 1) its must-fail fixtures; usage errors and unimplemented gates
# exit 2, never 0.
#   tools/m1-gates/selftest.sh
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
gate="$here/m1-gate.sh"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

printf '#!/bin/sh\necho 96bc0ada559d\n' >"$tmp/fab"
chmod +x "$tmp/fab"
printf 'review of 96bc0ada559d\nVERDICT: PASS\n' >"$tmp/pass.md"
printf 'review of 96bc0ada559d\nVERDICT: BLOCK\n' >"$tmp/block.md"
printf 'review of another head\nVERDICT: PASS\n' >"$tmp/other.md"
printf 'review of 96bc0ada559d\nVERDICT: PASS\nlater\nVERDICT: BLOCK\n' >"$tmp/flipped.md"

bad=0
expect() {
	want=$1; shift
	"$gate" "$@" >/dev/null 2>&1
	got=$?
	if [ "$got" != "$want" ]; then
		echo "selftest FAIL: m1-gate.sh $* -> $got, want $want"
		bad=1
	fi
}
expect 0 g0-review "$tmp/pass.md" "$tmp/fab"
expect 1 g0-review "$tmp/block.md" "$tmp/fab"
expect 1 g0-review "$tmp/other.md" "$tmp/fab"
expect 1 g0-review "$tmp/flipped.md" "$tmp/fab"
expect 2 g0-review "$tmp/missing.md" "$tmp/fab"
expect 0 g1-version "$tmp/fab" 96bc0ada559d3591878a7b73a926e79483cbc0ad
expect 1 g1-version "$tmp/fab" 7b563dfbb536
expect 2 g1-version "$tmp/fab" not-a-sha
for g in g2 g2b g3 g3-chaos g3b g4 g5 g6 g7 bogus; do
	expect 2 "$g"
done
expect 2
if [ "$bad" = 0 ]; then echo "selftest ok"; fi
exit "$bad"
