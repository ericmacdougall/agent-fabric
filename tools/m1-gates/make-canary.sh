#!/bin/bash
# goodqa#15966 (Fabric M1): build the canary repository that the first real
# mission diagnoses. The commit SHA is deterministic: content, author,
# committer and dates are fixed, so every host builds the same SHA and the
# gates can pin it.
#
#   tools/m1-gates/make-canary.sh DIR      create DIR (must not exist); print the SHA
#
# The repo is shell only, so a worker needs no toolchain: `./test.sh` fails
# because `sum_to` in lib.sh has an off-by-one (it stops one short of N). The
# mission asks the model to run the test and explain the failure; it must not
# need to edit anything. No personal data: all content is synthetic.
set -euo pipefail

dir=${1:?usage: make-canary.sh DIR}
if [ -e "$dir" ]; then
	echo "make-canary: $dir exists; refusing to overwrite" >&2
	exit 2
fi
mkdir -p "$dir"
cd "$dir"

cat >lib.sh <<'EOF'
#!/bin/sh
# sum_to N: print 1 + 2 + ... + N.
sum_to() {
	n=$1
	i=1
	total=0
	while [ "$i" -lt "$n" ]; do
		total=$((total + i))
		i=$((i + 1))
	done
	echo "$total"
}
EOF

cat >test.sh <<'EOF'
#!/bin/sh
# Run from the repo root: ./test.sh
. ./lib.sh
fail=0
check() {
	got=$(sum_to "$1")
	if [ "$got" != "$2" ]; then
		echo "FAIL: sum_to $1 = $got, want $2"
		fail=1
	else
		echo "ok: sum_to $1 = $got"
	fi
}
check 1 1
check 4 10
check 10 55
exit $fail
EOF

cat >README.md <<'EOF'
# m1-canary

Synthetic fixture for Fabric M1 (goodqa#15966). `./test.sh` fails. The task
is to find out why and explain it; nothing here needs to change.
EOF
chmod 0755 lib.sh test.sh

# Isolate from inherited Git configuration, templates and hooks (review
# spec-m1-v2 F4): no system or global config, an empty template directory,
# SHA-1 objects, no line-ending conversion or filters, no signing.
emptytpl=$(mktemp -d)
trap 'rmdir "$emptytpl" 2>/dev/null || true' EXIT
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_TEMPLATE_DIR="$emptytpl"
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_CONFIG GIT_CONFIG_PARAMETERS GIT_CONFIG_COUNT
export GIT_AUTHOR_NAME="fabric-m1" GIT_AUTHOR_EMAIL="m1-canary@invalid"
export GIT_COMMITTER_NAME="fabric-m1" GIT_COMMITTER_EMAIL="m1-canary@invalid"
export GIT_AUTHOR_DATE="2026-11-14T00:00:00Z" GIT_COMMITTER_DATE="2026-11-14T00:00:00Z"
git init -q --template="$emptytpl" --object-format=sha1 -b main .
gitc() {
	git -c core.autocrlf=false -c core.eol=lf -c core.safecrlf=false -c core.fileMode=true \
		-c core.hooksPath=/dev/null -c commit.gpgsign=false -c core.attributesFile=/dev/null "$@"
}
gitc add lib.sh test.sh README.md
gitc commit -q --no-verify -m "m1 canary fixture"
git rev-parse HEAD
