#!/bin/bash
# Failing controls for the codex adapter. Each mutant breaks one clause of the
# contract in a scratch copy of the module; the focused tests must FAIL on it.
# Prints "KILLED <name>: <failing tests>" per mutant and exits 1 if any mutant
# survives. GNU sed. Run on the dev host, never on a laptop:
#
#   internal/harness/codex/testdata/mutants.sh <module-root>
set -uo pipefail
root=${1:?module root}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
pkg=internal/harness/codex
survivors=0

mutant() { # name file sed-expression [append-text]
	local name=$1 file=$2 expr=$3 append=${4:-}
	if [ -n "${ONLY:-}" ] && ! [[ $name =~ $ONLY ]]; then return; fi
	rm -rf "$work/m"
	mkdir "$work/m"
	(cd "$root" && tar cf - --exclude=.git .) | (cd "$work/m" && tar xf -)
	sed -i "$expr" "$work/m/$pkg/$file"
	if cmp -s "$root/$pkg/$file" "$work/m/$pkg/$file"; then
		echo "BROKEN-CONTROL $name: the mutation did not change $file"
		survivors=$((survivors + 1))
		return
	fi
	[ -n "$append" ] && printf '%s\n' "$append" >>"$work/m/$pkg/$file"
	local out
	out=$(cd "$work/m" && timeout 300 go test -race -count=1 -timeout 120s "./$pkg/" 2>&1)
	if [ $? -eq 0 ]; then
		echo "SURVIVED $name"
		survivors=$((survivors + 1))
	else
		echo "KILLED $name: $(echo "$out" | grep -E '^--- FAIL|build failed|panic:' | sed -E 's/ \([0-9.]+s\)//' | sort -u | tr '\n' ' ')"
	fi
}

mutant drop-unknown-events actor.go 's/^func (s \*session) unknown(why, raw string) {$/&\n\treturn/'
mutant success-on-exit-without-result actor.go 's/Outcome: harness.OutcomeFailed, Reason: "codex exited/Outcome: harness.OutcomeDone, Reason: "codex exited/'
mutant exit-status-lost actor.go 's/ExitCode: e.code})/ExitCode: 0})/'
mutant skip-redaction codex.go 's/red = harness.PatternRedactor()/red = identRed{}/' 'type identRed struct{}
func (identRed) Redact(s string) string { return s }'
mutant inherit-caller-environment proc_unix.go 's/cmd.Env = append(\[\]string{}, env...)/cmd.Env = append(os.Environ(), env...)/'
mutant unbounded-lines lines.go 's/if size > l.max+1 {/if false \&\& size > l.max+1 {/'
mutant unbounded-total-output actor.go 's/if total > s.maxTotal {/if false \&\& total > s.maxTotal {/'
mutant approval-granted actor.go 's/"decision": "decline"/"decision": "accept"/'
mutant duplicate-keys-accepted identity.go 's/if _, dup := fields\[key\]; dup {/if _, dup := fields[key]; dup \&\& false {/'
mutant foreign-turn-accepted actor.go 's/case t.turnID == id:$/case true:/'
mutant interrupt-not-idempotent actor.go 's/if s.finished || t == nil || t.interrupt {/if s.finished || t == nil {/'
mutant queue-out-of-order actor.go 's/msg := s.queue\[0\]/msg := s.queue[len(s.queue)-1]/'
mutant stop-leaves-process actor.go 's/s.p.signalGroup(syscall.SIGTERM)/_ = syscall.SIGTERM/; s/s.p.signalGroup(syscall.SIGKILL)/_ = syscall.SIGKILL/'
mutant unrecorded-input-lost actor.go 's/if n := s.undelivered(); n > 0 {/if n := 0; n > 0 {/'

if [ "$survivors" -ne 0 ]; then
	echo "$survivors mutant(s) survived"
	exit 1
fi
echo "all mutants killed"
