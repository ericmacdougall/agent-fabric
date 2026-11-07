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
// settings, can carry neither environment nor a credential or command
// helper. Hooks run only single-path scripts from a hooks directory that
// the worker cannot write, reached by a path the worker cannot change.

// ErrSettings refuses a launch whose settings could hand the worker an
// environment, a credential helper or a command fabric does not control.
var ErrSettings = errors.New("claude settings refused")

// helperKeys run a command or script on the worker's behalf, or (policyHelper)
// compute settings at start-up (code.claude.com/docs/en/settings, read
// 2026-11-07).
var helperKeys = []string{"apiKeyHelper", "awsAuthRefresh", "awsCredentialExport", "gcpAuthRefresh", "otelHeadersHelper",
	"statusLine", "subagentStatusLine", "fileSuggestion", "policyHelper", "processWrapper", "sshHostCommands"}

// sandboxBinaryKeys, under "sandbox", name binaries Claude Code runs.
var sandboxBinaryKeys = []string{"ripgrep", "bwrapPath", "socatPath"}

// managedRefusedKeys add commands or code to every session on the machine:
// managed hooks (a SessionStart hook can extend the Bash environment),
// plugins and MCP servers. Inspection 6355639 F6.
var managedRefusedKeys = []string{"hooks", "enabledPlugins", "extraKnownMarketplaces", "managedMcpServers"}

// settingsKeys are the only top-level keys a --settings file may hold.
var settingsKeys = map[string]bool{"permissions": true, "hooks": true, "model": true}

// groupKeys and entryKeys are the exact keys of a hook group and a hook.
// Claude Code reads JSON case-sensitively; Go's struct decoding does not,
// so hooks are parsed as exact-key maps (inspection 6355639 F2).
var (
	groupKeys = map[string]bool{"matcher": true, "hooks": true}
	entryKeys = map[string]bool{"type": true, "command": true, "timeout": true}
)

// hookCommand is a single absolute path: no argument, no shell syntax.
var hookCommand = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)

// keyShape is a key that may be echoed in an error; anything else from an
// untrusted file is not repeated (inspection 6355639 F9).
var keyShape = regexp.MustCompile(`^[A-Za-z0-9_.$-]{1,64}$`)

func safeKey(k string) string {
	if keyShape.MatchString(k) {
		return k
	}
	return "(unprintable key)"
}

// DefaultManagedRoot is the system directory of managed-settings.json,
// managed-settings.d/ and managed-mcp.json
// (code.claude.com/docs/en/managed-settings, read 2026-11-07).
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
	if raw, ok := m["sandbox"]; ok {
		var sb map[string]json.RawMessage
		if err := json.Unmarshal(raw, &sb); err != nil {
			return refuse("%s: sandbox is not an object", path)
		}
		for _, k := range sandboxBinaryKeys {
			if _, ok := sb[k]; ok {
				return refuse("%s sets sandbox.%s", path, k)
			}
		}
	}
	return nil
}

// CheckManaged refuses a launch when the machine's managed settings (root,
// or the system directory when root is "") set env, a helper, hooks,
// plugins or MCP servers, or cannot be read or parsed. Managed settings
// apply whatever --setting-sources says. An absent root or file is
// accepted. The macOS managed-preferences plist and server-managed settings
// are not read (spec Risks: unchecked managed-settings sources).
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
		for _, k := range managedRefusedKeys {
			if _, ok := m[k]; ok {
				return refuse("%s sets %s", f, k)
			}
		}
	}
	return checkManagedMCP(filepath.Join(root, "managed-mcp.json"))
}

// checkManagedMCP refuses a managed-mcp.json that cannot be read or parsed,
// sets env or a helper at its top level, or defines any server: a managed
// MCP server is started for the worker whatever --strict-mcp-config says,
// and it inherits the worker's environment (FABRIC_DSN, FAB_RUN). An absent
// file, or one with an empty server map, is accepted.
func checkManagedMCP(path string) error {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return refuse("%s cannot be checked: %v", path, errors.Unwrap(err))
	}
	m, err := readObject(path)
	if err != nil {
		return err
	}
	if err := checkNoEnvOrHelper(path, m); err != nil {
		return err
	}
	raw, ok := m["mcpServers"]
	if !ok {
		return nil
	}
	var servers map[string]json.RawMessage
	if err := json.Unmarshal(raw, &servers); err != nil {
		return refuse("%s: mcpServers is not a server map", path)
	}
	if len(servers) > 0 {
		return refuse("%s defines %d MCP server(s), which would run in every worker", path, len(servers))
	}
	return nil
}

// CheckSettingsFile refuses a --settings file that the worker could change
// (see checkPath), that has a top-level key outside permissions, hooks and
// model (so env, every helper and statusLine are refused), or a hook that
// is not exactly {type: command, command: <single path inside hooksDir>}
// with an optional timeout. hooksDir and each hook must pass checkPath.
// Errors never repeat hook commands or unprintable keys from the file.
func CheckSettingsFile(path, hooksDir string, workerUID *int) error {
	if !filepath.IsAbs(path) {
		return refuse("the --settings path must be absolute")
	}
	if _, err := checkPath(path, path, workerUID, false); err != nil {
		return err
	}
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
			return refuse("%s sets %s, which is outside the allow-list (permissions, hooks, model)", path, safeKey(k))
		}
	}
	raw, ok := m["hooks"]
	if !ok {
		return nil
	}
	cmds, err := hookCommands(path, raw)
	if err != nil || len(cmds) == 0 {
		return err
	}
	root, err := checkHooksDir(hooksDir, workerUID)
	if err != nil {
		return err
	}
	configured := filepath.Clean(hooksDir)
	for _, h := range cmds {
		if !hookCommand.MatchString(h.command) || filepath.Clean(h.command) != h.command {
			return refuse("%s: %s is not a single absolute path", path, h.label)
		}
		// As written, since Claude Code runs the path as written; and as
		// resolved (inspection 6355639 F3).
		if !strings.HasPrefix(h.command, configured+string(filepath.Separator)) {
			return refuse("%s: %s is not under FABRIC_HOOKS_DIR as written", path, h.label)
		}
		real, err := checkPath(h.command, h.label, workerUID, false)
		if err != nil {
			return err
		}
		if !strings.HasPrefix(real, root+string(filepath.Separator)) {
			return refuse("%s: %s resolves outside FABRIC_HOOKS_DIR", path, h.label)
		}
	}
	return nil
}

type hookCmd struct{ label, command string }

// hookCommands parses the hooks event map with exact keys: an event maps
// to a list of groups {matcher?, hooks}, and each hook is {type, command,
// timeout?} with type "command". Any other key, a key differing only in
// case included, refuses.
func hookCommands(path string, raw json.RawMessage) ([]hookCmd, error) {
	var events map[string][]map[string]json.RawMessage
	if err := json.Unmarshal(raw, &events); err != nil {
		return nil, refuse("%s: hooks is not an event map of group lists", path)
	}
	names := make([]string, 0, len(events))
	for e := range events {
		names = append(names, e)
	}
	sort.Strings(names)
	var out []hookCmd
	for _, e := range names {
		ev := safeKey(e)
		for gi, g := range events[e] {
			for k := range g {
				if !groupKeys[k] {
					return nil, refuse("%s: %s group %d has key %s outside (matcher, hooks)", path, ev, gi, safeKey(k))
				}
			}
			if mr, ok := g["matcher"]; ok {
				var s string
				if json.Unmarshal(mr, &s) != nil {
					return nil, refuse("%s: %s group %d matcher is not a string", path, ev, gi)
				}
			}
			var entries []map[string]json.RawMessage
			if err := json.Unmarshal(g["hooks"], &entries); err != nil || entries == nil {
				return nil, refuse("%s: %s group %d hooks is not a list of hooks", path, ev, gi)
			}
			for hi, h := range entries {
				label := fmt.Sprintf("%s hook %d.%d", ev, gi, hi)
				for k := range h {
					if !entryKeys[k] {
						return nil, refuse("%s: %s has key %s outside (type, command, timeout)", path, label, safeKey(k))
					}
				}
				var typ, cmd string
				if json.Unmarshal(h["type"], &typ) != nil || typ != "command" {
					return nil, refuse("%s: %s type is not command", path, label)
				}
				if json.Unmarshal(h["command"], &cmd) != nil {
					return nil, refuse("%s: %s command is not a string", path, label)
				}
				if tr, ok := h["timeout"]; ok {
					var n float64
					if json.Unmarshal(tr, &n) != nil {
						return nil, refuse("%s: %s timeout is not a number", path, label)
					}
				}
				out = append(out, hookCmd{label, cmd})
			}
		}
	}
	return out, nil
}

// checkHooksDir returns the resolved hooks directory, refusing one that is
// unset, relative or unclean, or fails checkPath.
func checkHooksDir(dir string, workerUID *int) (string, error) {
	if dir == "" || !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return "", refuse("hooks need an absolute, clean FABRIC_HOOKS_DIR")
	}
	return checkPath(dir, "FABRIC_HOOKS_DIR", workerUID, true)
}

// checkPath refuses a settings file, hooks dir or hook (p, named label in
// errors) that the worker could change: on the path as written, a symlink
// not owned by root or fabric, or owned by the worker, or a directory that
// fails the ancestor rule (checkDir); then the resolved target must pass
// checkOwned and every directory above it the ancestor rule. It returns the
// resolved path.
func checkPath(p, label string, workerUID *int, wantDir bool) (string, error) {
	if err := checkWritten(p, label, workerUID); err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", refuse("%s cannot be resolved", label)
	}
	if err := checkOwned(real, label, workerUID, wantDir); err != nil {
		return "", err
	}
	return real, checkAncestors(filepath.Dir(real), label, workerUID)
}

// checkWritten walks p as written, from p up to "/", without following
// its last component: a symlink there could be repointed by its owner, so
// it must be root's or fabric's and never the worker's.
func checkWritten(p, label string, workerUID *int) error {
	for cur := p; ; {
		fi, err := os.Lstat(cur)
		if err != nil {
			return refuse("%s: a path component cannot be checked", label)
		}
		uid, ok := owner(fi)
		if !ok {
			return refuse("%s: a path component has no known owner", label)
		}
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			if (workerUID != nil && uid == *workerUID) || (uid != 0 && uid != os.Getuid()) {
				return refuse("%s: a symlink on its path is owned by uid %d, not root or fabric", label, uid)
			}
		case fi.IsDir():
			if err := checkDir(fi, uid, label, workerUID); err != nil {
				return err
			}
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return nil
		}
		cur = parent
	}
}

// checkAncestors refuses when dir or any directory above it fails checkDir.
func checkAncestors(dir, label string, workerUID *int) error {
	for {
		fi, err := os.Stat(dir)
		if err != nil {
			return refuse("%s: a directory above it cannot be checked", label)
		}
		uid, ok := owner(fi)
		if !ok {
			return refuse("%s: a directory above it has no known owner", label)
		}
		if err := checkDir(fi, uid, label, workerUID); err != nil {
			return err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil
		}
		dir = parent
	}
}

// checkDir is the ancestor rule: a directory on the way to a settings file
// or hook must not be owned by the worker user, nor writable by group or
// others unless it is sticky and root-owned (like /tmp), or the worker
// could rename a component and swap what the path reaches.
func checkDir(fi os.FileInfo, uid int, label string, workerUID *int) error {
	if workerUID != nil && uid == *workerUID {
		return refuse("%s: a directory on its path is owned by the worker user", label)
	}
	if fi.Mode().Perm()&0o022 != 0 && !(fi.Mode()&os.ModeSticky != 0 && uid == 0) {
		return refuse("%s: a directory on its path is writable by group or others (%v)", label, fi.Mode())
	}
	return nil
}

func owner(fi os.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}

// checkOwned refuses a settings file, hooks directory or hook script that
// is writable by group or others, owned by the worker user, or owned by
// anyone but root or this process: the worker could otherwise change what
// it says or runs. Claude Code reloads an edited settings file in a running
// session, so this applies to the settings file too (inspection 6355639 F5).
func checkOwned(path, label string, workerUID *int, wantDir bool) error {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() != wantDir {
		return refuse("%s is not a %s", label, map[bool]string{true: "directory", false: "file"}[wantDir])
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return refuse("%s is writable by group or others (%v)", label, fi.Mode().Perm())
	}
	uid, ok := owner(fi)
	if !ok {
		return refuse("%s owner unknown", label)
	}
	if workerUID != nil && uid == *workerUID {
		return refuse("%s is owned by the worker user", label)
	}
	if uid != 0 && uid != os.Getuid() {
		return refuse("%s is owned by uid %d, not root or fabric", label, uid)
	}
	return nil
}
