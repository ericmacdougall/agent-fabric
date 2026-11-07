package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"syscall"
)

// goodqa#15087 AC-6 (c): a claude-code worker loads no user or project
// settings source (`--setting-sources ""`), and the settings that still
// apply, the --settings file fabric passes and the machine's managed
// settings, can carry neither environment nor a credential or status
// helper. Hooks run only single-path scripts from a hooks directory that
// the worker cannot write.

// ErrSettings refuses a launch whose settings could hand the worker an
// environment, a credential helper or a command fabric does not control.
var ErrSettings = errors.New("claude settings refused")

// helperKeys run a command or script on the worker's behalf.
var helperKeys = []string{"apiKeyHelper", "awsAuthRefresh", "awsCredentialExport", "gcpAuthRefresh", "otelHeadersHelper", "statusLine", "fileSuggestion"}

// settingsKeys are the only top-level keys a --settings file may hold.
var settingsKeys = map[string]bool{"$schema": true, "permissions": true, "hooks": true}

// hookCommand is a single absolute path: no argument, no shell syntax.
var hookCommand = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)

// DefaultManagedRoot is the system directory of managed-settings.json and
// managed-settings.d/ (code.claude.com/docs/en/managed-settings, read
// 2026-11-07).
func DefaultManagedRoot() string {
	if runtime.GOOS == "darwin" {
		return "/Library/Application Support/ClaudeCode"
	}
	return "/etc/claude-code"
}

func refuse(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrSettings}, a...)...)
}

// readObject reads one settings document as a JSON object. A file that
// exists but cannot be read or parsed refuses, as Claude Code itself
// refuses to start on an unparsable managed document.
func readObject(path string) (map[string]json.RawMessage, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, refuse("%s cannot be read: %v", path, errors.Unwrap(err))
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil || m == nil {
		return nil, refuse("%s is not a JSON object", path)
	}
	return m, nil
}

func checkNoEnvOrHelper(path string, m map[string]json.RawMessage) error {
	if _, ok := m["env"]; ok {
		return refuse("%s sets env", path)
	}
	for _, k := range helperKeys {
		if _, ok := m[k]; ok {
			return refuse("%s sets %s", path, k)
		}
	}
	return nil
}

// CheckManaged refuses a launch when the machine's managed settings (root,
// or the system directory when root is "") set env or a helper, or cannot
// be read or parsed. Managed settings apply whatever --setting-sources
// says. An absent root or file is accepted.
func CheckManaged(root string) error {
	if root == "" {
		root = DefaultManagedRoot()
	}
	files := []string{}
	main := filepath.Join(root, "managed-settings.json")
	if _, err := os.Lstat(main); err == nil {
		files = append(files, main)
	} else if !errors.Is(err, os.ErrNotExist) {
		return refuse("%s cannot be checked: %v", main, errors.Unwrap(err))
	}
	dropins := filepath.Join(root, "managed-settings.d")
	if _, err := os.Lstat(dropins); err == nil {
		ents, err := os.ReadDir(dropins)
		if err != nil {
			return refuse("%s cannot be read: %v", dropins, errors.Unwrap(err))
		}
		names := []string{}
		for _, e := range ents {
			if strings.HasSuffix(e.Name(), ".json") {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		for _, n := range names {
			files = append(files, filepath.Join(dropins, n))
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return refuse("%s cannot be checked: %v", dropins, errors.Unwrap(err))
	}
	for _, f := range files {
		m, err := readObject(f)
		if err != nil {
			return err
		}
		if err := checkNoEnvOrHelper(f, m); err != nil {
			return err
		}
	}
	return nil
}

// CheckSettingsFile refuses a --settings file with a top-level key outside
// $schema, permissions and hooks (so env, every helper and statusLine are
// refused), or a hook that is not a single path inside hooksDir. hooksDir
// must not be writable by group or others, nor owned by the worker
// (workerUID, when known); it must be owned by root or by this process.
func CheckSettingsFile(path, hooksDir string, workerUID *int) error {
	m, err := readObject(path)
	if err != nil {
		return err
	}
	if err := checkNoEnvOrHelper(path, m); err != nil {
		return err
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !settingsKeys[k] {
			return refuse("%s sets %s, which is outside the allow-list ($schema, permissions, hooks)", path, k)
		}
	}
	raw, ok := m["hooks"]
	if !ok {
		return nil
	}
	var hooks map[string][]struct {
		Matcher string `json:"matcher"`
		Hooks   []struct {
			Type    string `json:"type"`
			Command string `json:"command"`
			Timeout int    `json:"timeout"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &hooks); err != nil {
		return refuse("%s: hooks is not an event map", path)
	}
	if len(hooks) == 0 {
		return nil
	}
	root, err := checkHooksDir(hooksDir, workerUID)
	if err != nil {
		return err
	}
	for event, groups := range hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				if h.Type != "command" {
					return refuse("%s: %s hook type %q is not command", path, event, h.Type)
				}
				if !hookCommand.MatchString(h.Command) || filepath.Clean(h.Command) != h.Command {
					return refuse("%s: %s hook %q is not a single absolute path", path, event, h.Command)
				}
				real, err := filepath.EvalSymlinks(h.Command)
				if err != nil {
					return refuse("%s: %s hook %q cannot be resolved", path, event, h.Command)
				}
				if !strings.HasPrefix(real, root+string(filepath.Separator)) {
					return refuse("%s: %s hook %q resolves outside FABRIC_HOOKS_DIR", path, event, h.Command)
				}
				if err := checkOwned(real, workerUID, false); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// checkHooksDir returns the resolved hooks directory, refusing one that is
// unset or fails checkOwned.
func checkHooksDir(dir string, workerUID *int) (string, error) {
	if dir == "" || !filepath.IsAbs(dir) {
		return "", refuse("hooks need an absolute FABRIC_HOOKS_DIR")
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", refuse("FABRIC_HOOKS_DIR cannot be resolved")
	}
	return real, checkOwned(real, workerUID, true)
}

// checkOwned refuses a hooks directory or hook script that is writable by
// group or others, owned by the worker user, or owned by anyone but root or
// this process: the worker could otherwise change what a hook runs.
func checkOwned(path string, workerUID *int, wantDir bool) error {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() != wantDir {
		return refuse("%s is not a %s", path, map[bool]string{true: "directory", false: "file"}[wantDir])
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return refuse("%s is writable by group or others (%v)", path, fi.Mode().Perm())
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return refuse("%s owner unknown", path)
	}
	uid := int(st.Uid)
	if workerUID != nil && uid == *workerUID {
		return refuse("%s is owned by the worker user", path)
	}
	if uid != 0 && uid != os.Getuid() {
		return refuse("%s is owned by uid %d, not root or fabric", path, uid)
	}
	return nil
}
