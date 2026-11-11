package claudecode

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

// InputIdentity binds reviewed source bytes, never an approval or clearance.
// Callers must separately establish managed sources and native effective policy.
type InputIdentity struct {
	PolicySHA string
	HooksSHA  string
}

// FixedInputSnapshot reduces the existing user settings privately to mandatory
// hook definitions only. It never returns unrelated config/env/account fields.
// Only the exact reviewed current policy is appended; automatic imports are
// rejected rather than followed. This demo has an empty approved import graph.
func FixedInputSnapshot(policyPath, settingsPath string, identity InputIdentity) ([]byte, []byte, error) {
	refuse := func() ([]byte, []byte, error) {
		return nil, nil, errors.New("claudecode: instruction or hook identity refused")
	}
	read := func(p string, max int64) ([]byte, error) {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p {
			return nil, errors.New("invalid source")
		}
		for d := p; ; d = filepath.Dir(d) {
			st, e := os.Lstat(d)
			if e != nil || st.Mode()&os.ModeSymlink != 0 {
				return nil, errors.New("invalid source ancestry")
			}
			if d == p && (!st.Mode().IsRegular() || st.Size() > max) {
				return nil, errors.New("invalid source size")
			}
			if d == filepath.Dir(d) {
				break
			}
		}
		b, e := os.ReadFile(p)
		if int64(len(b)) > max {
			return nil, errors.New("source changed size")
		}
		return b, e
	}
	policy, err := read(policyPath, 64<<10)
	if err != nil {
		return refuse()
	}
	sum := sha256.Sum256(policy)
	if len(identity.PolicySHA) != 64 || hex.EncodeToString(sum[:]) != identity.PolicySHA || strings.Contains(string(policy), "@") {
		return refuse()
	}
	// The documented server-policy cache is configuration, not a conversation
	// source. Do not let an unreviewed cached helper/env/hook execute even in
	// the metadata doctor command. Current demo permits absent or exactly {}.
	cachePath := filepath.Join(filepath.Dir(settingsPath), "remote-settings.json")
	if _, e := os.Lstat(cachePath); !errors.Is(e, os.ErrNotExist) {
		cache, e := read(cachePath, 64<<10)
		if e != nil {
			return refuse()
		}
		var object map[string]json.RawMessage
		if strictJSON(cache, &object) != nil || object == nil || len(object) != 0 {
			return refuse()
		}
	}
	raw, err := read(settingsPath, 256<<10)
	if err != nil {
		return refuse()
	}
	// Duplicate keys are rejected before extracting the nonsecret hook subtree.
	var config map[string]json.RawMessage
	if strictJSON(raw, &config) != nil {
		return refuse()
	}
	var hooks map[string]any
	if json.Unmarshal(config["hooks"], &hooks) != nil || len(hooks) == 0 {
		return refuse()
	}
	canonical, err := json.Marshal(hooks)
	if err != nil {
		return refuse()
	}
	sum = sha256.Sum256(canonical)
	if len(identity.HooksSHA) != 64 || hex.EncodeToString(sum[:]) != identity.HooksSHA {
		return refuse()
	}
	settings, err := json.Marshal(map[string]any{
		"hooks":             hooks,
		"autoMemoryEnabled": false,
		"claudeMdExcludes":  []string{"**/CLAUDE.md", "**/CLAUDE.local.md", "**/AGENTS.md", "**/.claude/rules/**"},
		"pluginConfigs":     map[string]any{"cc-plugin-agents-md@builtin": map[string]any{"options": map[string]string{"instructionFiles": "managed-only"}}},
		"permissions":       map[string]any{"ask": []string{"Edit"}, "deny": []string{"Bash", "Agent", "Task", "WebFetch", "WebSearch", "mcp__*"}},
	})
	if err != nil {
		return refuse()
	}
	return policy, settings, nil
}

// FixedEnvironment is literal, scoped to one native process and its required
// trusted hooks. No shell profile, API key, provider or root-CA env is inherited.
func FixedEnvironment(home string) map[string]string {
	return map[string]string{
		"PATH": "/usr/bin:/bin:/opt/homebrew/bin", "HOME": home, "USER": "eric", "LANG": "C.UTF-8", "LC_ALL": "C", "SHELL": "/bin/sh",
		"PYTHONNOUSERSITE": "1", "PYTHONDONTWRITEBYTECODE": "1",
		"CLAUDE_CODE_DISABLE_CLAUDE_MDS": "1", "CLAUDE_CODE_DISABLE_AUTO_MEMORY": "1", "CLAUDE_CODE_DISABLE_ATTACHMENTS": "1",
		// Optional detached backup/prune maintenance; WAL/security hooks stay on.
		"AGENT_HARNESS_TASK_OUTPUT_PRUNE": "off",
		"DISABLE_TELEMETRY":               "1", "DISABLE_ERROR_REPORTING": "1",
		"DISABLE_AUTOUPDATER": "1", "ENABLE_CLAUDEAI_MCP_SERVERS": "false",
	}
}

// CheckFixedOwner binds the nonsecret native-store account name to the actual
// process UID and real home owner before any authentication metadata command.
func CheckFixedOwner(home string) error {
	current, err := user.Current()
	if err != nil {
		return errors.New("claudecode: owned user identity unavailable")
	}
	st, err := os.Lstat(home)
	if err != nil {
		return errors.New("claudecode: owned user home unavailable")
	}
	metadata, ok := st.Sys().(*syscall.Stat_t)
	if !ok || !fixedOwnerMatches(home, current, os.Getuid(), metadata.Uid, st.IsDir() && st.Mode()&os.ModeSymlink == 0) {
		return errors.New("claudecode: owned user identity refused")
	}
	return nil
}

func fixedOwnerMatches(home string, current *user.User, uid int, homeUID uint32, realDirectory bool) bool {
	if current == nil {
		return false
	}
	accountUID, err := strconv.Atoi(current.Uid)
	return err == nil && current.Username == "eric" && current.HomeDir == home && filepath.IsAbs(home) && filepath.Clean(home) == home && accountUID == uid && homeUID == uint32(uid) && realDirectory
}

// FixedArguments accepts only internally generated paths and an owned UUID.
// It preserves the native default prompt and executes no shell wrapper.
func FixedArguments(id, settings, policy, mcp string) ([]string, error) {
	if !uuidRE.MatchString(id) {
		return nil, errors.New("claudecode: owned UUID required")
	}
	for _, p := range []string{settings, policy, mcp} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p || strings.ContainsAny(p, "\x00\r\n") {
			return nil, errors.New("claudecode: invalid control file")
		}
	}
	return []string{"--print", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--restricted", "--tools", "Write", "--permission-mode", "manual", "--permission-prompts", "host", "--permission-prompt-tool", "stdio",
		"--strict-mcp-config", "--mcp-config", mcp, "--disable-slash-commands", "--include-hook-events", "--no-session-persistence",
		"--session-id", id, "--settings", settings, "--append-system-prompt-file", policy, "--model", "sonnet", "--effort", "low", "--max-turns", "2"}, nil
}

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
