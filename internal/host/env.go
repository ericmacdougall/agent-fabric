package host

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// envName is the shape of a variable name fabric sets for a worker. A
// comma is never part of one, so a name list joins on commas safely.
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// WorkerEnvNames is the allow-list of variables fabric may set for a worker
// (goodqa#15087 AC-6): the host profile's own (FAB_RUN, HOME, PATH, LANG and,
// on Darwin, USER, TMPDIR, SHELL), the claude launch's private config and
// cache paths and run identity, and the scheduler's run paths and runner
// DSN. A name outside it is refused, so a new variable is a reviewed change.
var WorkerEnvNames = []string{
	"CLAUDE_CODE_DISABLE_CLAUDE_MDS", "CLAUDE_CONFIG_DIR",
	"FAB_ATTEMPT", "FAB_FENCE", "FAB_MISSION", "FAB_RUN", "FAB_RUN_DIR", "FAB_WORKTREE",
	"FABRIC_DSN", "FABRIC_HOME",
	"GOCACHE", "GOFLAGS", "GOMODCACHE",
	"HOME", "LANG", "PATH", "SHELL", "TMPDIR", "USER",
	"XDG_CACHE_HOME", "XDG_CONFIG_HOME",
}

func allowedName(n string) bool {
	for _, a := range WorkerEnvNames {
		if a == n {
			return true
		}
	}
	return false
}

// WorkerEnv is the exact environment of a worker (goodqa#15087 AC-6):
// fabric's NAME=VALUE pairs, a later pair for a name replacing an earlier
// one, in first-seen order, plus the sorted names. Nothing else reaches the
// worker; on Linux `fab __sandbox --env-keep` drops whatever the worker
// user's systemd manager holds.
func WorkerEnv(pairs []string) (env, names []string, err error) {
	idx := map[string]int{}
	for _, p := range pairs {
		name, _, ok := strings.Cut(p, "=")
		if !ok || !envName.MatchString(name) {
			return nil, nil, fmt.Errorf("%w: environment entry %q is not NAME=VALUE", ErrGrantInvalid, name)
		}
		if !allowedName(name) {
			return nil, nil, fmt.Errorf("%w: %s is not on the worker environment allow-list", ErrGrantInvalid, name)
		}
		if i, seen := idx[name]; seen {
			env[i] = p
			continue
		}
		idx[name] = len(env)
		env = append(env, p)
	}
	for n := range idx {
		names = append(names, n)
	}
	sort.Strings(names)
	return env, names, nil
}

// KeepEnv filters environ to the listed names: what `fab __sandbox
// --env-keep` execs the worker with.
func KeepEnv(environ []string, keep string) ([]string, error) {
	want := map[string]bool{}
	for _, n := range strings.Split(keep, ",") {
		if !envName.MatchString(n) {
			return nil, fmt.Errorf("--env-keep: %q is not a variable name", n)
		}
		want[n] = true
	}
	var out []string
	for _, e := range environ {
		if name, _, ok := strings.Cut(e, "="); ok && want[name] {
			out = append(out, e)
		}
	}
	return out, nil
}
