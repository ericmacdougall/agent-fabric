package host

import (
	"fmt"
	"io"
	"os"
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
	"GIT_CONFIG_GLOBAL",
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

// managerProxyEnvNames are set only by the Linux worker manager, from its
// per-run egress mediator (goodqa#15235 S5b). WorkerEnv still refuses them
// from callers; an environment file may carry them.
var managerProxyEnvNames = []string{
	"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
	"http_proxy", "https_proxy", "all_proxy", "no_proxy",
}

func envFileName(name string) bool {
	if allowedName(name) {
		return true
	}
	for _, n := range managerProxyEnvNames {
		if n == name {
			return true
		}
	}
	return false
}

// maxEnvFile bounds a worker environment file.
const maxEnvFile = 1 << 20

// WriteEnvFile writes a worker's NAME=VALUE pairs, NUL-separated, to a new
// file (goodqa#15177: values never go on a command line, where sudo and
// systemd-run would show them to every local account for the whole run).
// The file must not exist and is never followed through a symlink. Mode is
// 0600, or 0640 when a separate worker account reads it through the run
// directory's group.
func WriteEnvFile(path string, pairs []string, groupRead bool) error {
	mode := os.FileMode(0o600)
	if groupRead {
		mode = 0o640
	}
	f, err := openEnvFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("worker environment file: %w", err)
	}
	err = f.Chmod(mode) // the umask may have narrowed it
	if err == nil {
		_, err = f.WriteString(strings.Join(pairs, "\x00"))
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
		return fmt.Errorf("worker environment file: %w", err)
	}
	return nil
}

// ReadEnvFile reads and removes a worker environment file: what `fab
// __sandbox --env-file` execs the worker with. Every entry must be
// NAME=VALUE with an allow-listed name listed in keep, at most once, and
// every name in keep must be present, so the file and the launcher's name
// list agree exactly. The file is unlinked right after it is read, before
// validation, and the open file must then have no links left (an inode check,
// so a rename or hard link cannot keep the secrets on disk). A failed cleanup
// refuses the exec (goodqa#15235 S5a).
func ReadEnvFile(path, keep string) ([]string, error) {
	f, err := openEnvFile(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("--env-file: %w", err)
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() || st.Size() > maxEnvFile {
		return nil, fmt.Errorf("--env-file: not a regular file of at most %d bytes", maxEnvFile)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxEnvFile+1))
	if err != nil {
		return nil, fmt.Errorf("--env-file: %w", err)
	}
	if err := os.Remove(path); err != nil {
		return nil, fmt.Errorf("--env-file unlink before exec: %w", err)
	}
	if n, err := envFileLinks(f); err != nil || n != 0 {
		if err == nil {
			err = fmt.Errorf("%d links remain after unlink", n)
		}
		return nil, fmt.Errorf("--env-file verify removal before exec: %w", err)
	}
	want := map[string]bool{}
	for _, n := range strings.Split(keep, ",") {
		if !envName.MatchString(n) {
			return nil, fmt.Errorf("--env-keep: %q is not a variable name", n)
		}
		want[n] = true
	}
	seen := map[string]bool{}
	var out []string
	if len(b) > 0 {
		for _, e := range strings.Split(string(b), "\x00") {
			name, _, ok := strings.Cut(e, "=")
			if !ok || !envName.MatchString(name) || !envFileName(name) || !want[name] || seen[name] {
				return nil, fmt.Errorf("--env-file: entry %q is not an expected NAME=VALUE", name)
			}
			seen[name] = true
			out = append(out, e)
		}
	}
	if len(seen) != len(want) {
		return nil, fmt.Errorf("--env-file: %d entries, want the %d names of --env-keep", len(seen), len(want))
	}
	return out, nil
}
