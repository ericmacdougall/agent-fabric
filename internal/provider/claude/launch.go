package claude

import (
	"fmt"
	"strconv"
	"strings"

	"fabric/internal/host"
)

// Launch is the host-side configuration of a worker that is not part of the
// grant: where the binary is, which settings and brief files were
// materialised, and the prompt (the compact checkpoint).
type Launch struct {
	Binary          string   // path to the claude CLI
	Prompt          string   // the compact checkpoint
	SessionID       string   // fabric-minted uuid; "" lets the CLI mint one
	SettingSources  string   // e.g. "user" during migration, "" for none
	SettingsPath    string   // worker profile settings file
	BriefPath       string   // --append-system-prompt-file
	SchemaPath      string   // --json-schema; when set, a result without structured_output is abandoned
	Tools           []string // --tools allow-list; nil keeps the CLI default
	DisallowedTools []string
	StrictMCP       bool
	DisableSlash    bool
	PermissionMode  string
}

// Argv builds the launch line from the grant and the launch config. It is the
// only place the model and effort flags are written, and both come from the
// grant: a grant without a model is refused before any argv exists (R23).
func Argv(g host.Grant, l Launch) (argv []string, env []string, err error) {
	if err := g.Validate(); err != nil {
		return nil, nil, err
	}
	if l.Binary == "" || l.Prompt == "" {
		return nil, nil, fmt.Errorf("%w: launch needs a binary and a prompt", host.ErrGrantInvalid)
	}
	mode := l.PermissionMode
	if mode == "" {
		mode = "acceptEdits"
	}
	argv = []string{l.Binary, "-p", l.Prompt,
		"--output-format", "stream-json", "--verbose",
		"--model", g.Model, "--effort", g.Effort,
		"--max-turns", strconv.Itoa(g.MaxTurns),
		"--max-budget-usd", strconv.FormatFloat(g.MaxBudgetUSD, 'f', 2, 64),
		"--permission-mode", mode,
		"--exclude-dynamic-system-prompt-sections",
	}
	if l.SessionID != "" {
		argv = append(argv, "--session-id", l.SessionID)
	}
	if l.SettingSources != "" {
		argv = append(argv, "--setting-sources", l.SettingSources)
	} else {
		argv = append(argv, "--setting-sources", "")
	}
	if l.SettingsPath != "" {
		argv = append(argv, "--settings", l.SettingsPath)
	}
	if l.BriefPath != "" {
		argv = append(argv, "--append-system-prompt-file", l.BriefPath)
	}
	if l.SchemaPath != "" {
		argv = append(argv, "--json-schema", l.SchemaPath)
	}
	if l.Tools != nil {
		argv = append(argv, "--tools", strings.Join(l.Tools, ","))
	}
	if len(l.DisallowedTools) > 0 {
		argv = append(argv, "--disallowedTools", strings.Join(l.DisallowedTools, ","))
	}
	if l.StrictMCP {
		argv = append(argv, "--strict-mcp-config")
	}
	if l.DisableSlash {
		argv = append(argv, "--disable-slash-commands")
	}
	env = []string{
		"CLAUDE_CODE_DISABLE_CLAUDE_MDS=1",
		"FAB_RUN=" + g.Token,
		"FAB_MISSION=" + string(g.Mission),
		"FAB_ATTEMPT=" + string(g.Run),
		"FAB_FENCE=" + strconv.FormatInt(g.Fence, 10),
	}
	return argv, env, nil
}
