package mission

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
)

// BashGateName is the hook executable's name under FABRIC_HOOKS_DIR: a hard
// link (or copy) of the sealed fab binary, which runs the gate when invoked
// by this name (a hook command takes no arguments).
const BashGateName = "fab-bash-gate"

// PermissionMode is the launch's mode: every call that would prompt is
// denied in -p mode (AC-24 "everything else is denied").
const PermissionMode = "dontAsk"

// GateTimeoutSeconds bounds one Bash gate run.
const GateTimeoutSeconds = 30

// readOnlyBash lists the commands Claude Code runs without a prompt in every
// mode (code.claude.com/docs/en/permissions, "Read-only commands", read
// 2026-11-14), plus environment printers, shells, interpreters and network
// clients. The list is defence in depth: the docs say the built-in set
// "includes" these, so it cannot be proved complete. The PreToolUse gate
// (BashGate) is what makes "exactly Bash(./test.sh)" hold.
var readOnlyBash = []string{
	"ls", "cat", "echo", "pwd", "head", "tail", "grep", "egrep", "fgrep", "rg", "find", "wc", "which", "diff", "stat", "du", "df", "cd", "git",
	"file", "ps", "sort", "uniq", "cut", "tr", "awk", "sed", "less", "more", "od", "xxd", "hexdump", "base64", "strings", "readlink", "realpath",
	"basename", "dirname", "tree", "id", "whoami", "uname", "hostname", "date", "printf", "type", "command", "test", "true", "false",
	"env", "printenv", "set", "export", "declare", "tee", "xargs", "exec", "eval", "source", ".", "time", "timeout", "nohup", "nice", "sudo",
	"sh", "bash", "zsh", "dash", "ksh", "python", "python3", "perl", "ruby", "node",
	"curl", "wget", "nc", "ncat", "socat", "ssh", "scp", "openssl",
}

// safePath is a worktree path that is literal in a gitignore pattern.
var safePath = regexp.MustCompile(`^(/[A-Za-z0-9._-]+)+$`)

// Settings is the --settings document of one attempt: the pinned allow
// list, the deny list, reads blocked outside the working directory (the
// worktree), and the Bash gate hook. worktree and hooksDir are absolute.
func Settings(worktree, hooksDir string) ([]byte, error) {
	for name, p := range map[string]string{"worktree": worktree, "hooks dir": hooksDir} {
		if !safePath.MatchString(p) || filepath.Clean(p) != p {
			return nil, fmt.Errorf("%w: %s path is not a literal absolute path", ErrConfig, name)
		}
	}
	deny := []string{"Read(//proc/**)", "Read(//sys/**)", "Read(//proc)", "Read(//sys)"}
	for _, c := range readOnlyBash {
		deny = append(deny, "Bash("+c+" *)")
	}
	allow := append(append([]string(nil), PinnedAllow...), "Read(/"+worktree+"/**)")
	doc := map[string]any{
		"permissions": map[string]any{
			"defaultMode":                         PermissionMode,
			"allow":                               allow,
			"deny":                                deny,
			"blockReadsOutsideWorkingDirectories": true,
			"disableBypassPermissionsMode":        "disable",
		},
		"hooks": map[string]any{
			"PreToolUse": []any{map[string]any{
				"matcher": "Bash",
				// onFailure block: a gate that cannot start, times out or
				// exits other than 0 or 2 blocks the call (Claude Code
				// v2.1.295 or later; code.claude.com/docs/en/hooks).
				"hooks": []any{map[string]any{"type": "command", "command": filepath.Join(hooksDir, BashGateName),
					"timeout": GateTimeoutSeconds, "onFailure": "block"}},
			}},
		},
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
