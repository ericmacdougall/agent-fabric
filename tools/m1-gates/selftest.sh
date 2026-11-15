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

S=96bc0ada559d3591878a7b73a926e79483cbc0ad
O=7b563dfbb5360000000000000000000000000000
mk() { printf '#!/bin/sh\n%s\n' "$2" >"$tmp/$1"; chmod +x "$tmp/$1"; }
mk fab 'echo 96bc0ada559d'
mk fabfull "echo $S"
mk fabrc 'echo 96bc0ada559d; exit 3'
mk fabshort 'echo 9'
mk fabjunk 'echo dev-build'
mk fabempty 'true'
mk fabmulti 'echo 96bc0ada559d; echo extra'
mk fabspace 'echo 96bc0ada 559d'
mk fablead 'echo " 96bc0ada559d"'
printf 'Reviewed HEAD: %s\nbody\nVERDICT: PASS\n\n' "$S" >"$tmp/pass.md"
printf 'Reviewed HEAD: %s\nVERDICT: BLOCK\n' "$S" >"$tmp/block.md"
printf 'Reviewed HEAD: %s\nbackground mentions %s\nVERDICT: PASS\n' "$O" "$S" >"$tmp/mixed.md"
printf 'review of %s with no field\nVERDICT: PASS\n' "$S" >"$tmp/nofield.md"
printf 'Reviewed HEAD: %s\nReviewed HEAD: %s\nVERDICT: PASS\n' "$S" "$O" >"$tmp/two.md"
printf 'Reviewed HEAD: %s\nVERDICT: PASS\nOne more objection: AC-7 is unsafe.\n' "$S" >"$tmp/trailing.md"
printf 'Reviewed HEAD: %s\nVERDICT: PASS\n' "${S:0:12}" >"$tmp/short.md"

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
expect 0 g0-review "$tmp/pass.md" "$tmp/fab" "$S"
expect 0 g0-review "$tmp/pass.md" "$tmp/fabfull" "$S"
expect 1 g0-review "$tmp/block.md" "$tmp/fab" "$S"
expect 1 g0-review "$tmp/mixed.md" "$tmp/fab" "$S"
expect 1 g0-review "$tmp/nofield.md" "$tmp/fab" "$S"
expect 1 g0-review "$tmp/two.md" "$tmp/fab" "$S"
expect 1 g0-review "$tmp/trailing.md" "$tmp/fab" "$S"
expect 1 g0-review "$tmp/short.md" "$tmp/fab" "$S"
expect 1 g0-review "$tmp/pass.md" "$tmp/fabrc" "$S"
expect 1 g0-review "$tmp/pass.md" "$tmp/fab" "$O"
expect 2 g0-review "$tmp/pass.md" "$tmp/fab" 96bc0ada559d
expect 2 g0-review "$tmp/missing.md" "$tmp/fab" "$S"
expect 0 g1-version "$tmp/fab" "$S"
expect 0 g1-version "$tmp/fabfull" "$S"
expect 1 g1-version "$tmp/fab" "$O"
expect 1 g1-version "$tmp/fabrc" "$S"
expect 1 g1-version "$tmp/fabshort" "$S"
expect 1 g1-version "$tmp/fabjunk" "$S"
expect 1 g1-version "$tmp/fabempty" "$S"
expect 1 g1-version "$tmp/fabmulti" "$S"
expect 1 g1-version "$tmp/fabspace" "$S"
expect 1 g1-version "$tmp/fablead" "$S"
expect 1 g0-review "$tmp/pass.md" "$tmp/fabmulti" "$S"
expect 2 g1-version "$tmp/fab" 96bc0ada559d
expect 2 g1-version "$tmp/fab" not-a-sha
for g in g2 g2b g3 g3-chaos g3b g4 g5 g6 g7 bogus; do
	expect 2 "$g"
done
expect 2
if [ "$bad" = 0 ]; then echo "selftest ok"; fi
exit "$bad"
