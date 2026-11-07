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
	SettingSources  string   // must be "": no user or project settings source (goodqa#15087 AC-6)
	SettingsPath    string   // worker profile settings file
	BriefPath       string   // --append-system-prompt-file
	SchemaPath      string   // --json-schema; when set, a result without structured_output is abandoned
	Tools           []string // --tools allow-list; nil keeps the CLI default
	DisallowedTools []string
	StrictMCP       bool
	DisableSlash    bool
	PermissionMode  string
	HooksDir        string // FABRIC_HOOKS_DIR: the only place a --settings hook may run from
	WorkerUID       *int   // the worker account; it must not own the hooks
	ManagedRoot     string // managed-settings directory; "" is the system one
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
	if l.SettingSources != "" {
		return nil, nil, refuse("setting sources %q: a worker loads no user or project settings", l.SettingSources)
	}
	if err := CheckManaged(l.ManagedRoot); err != nil {
		return nil, nil, err
	}
	if l.SettingsPath != "" {
		if err := CheckSettingsFile(l.SettingsPath, l.HooksDir, l.WorkerUID); err != nil {
			return nil, nil, err
		}
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
	argv = append(argv, "--setting-sources", "")
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
	// Private configuration and caches under the run dir (review C2): the
	// user's ~/.claude (hooks, settings, agents) and ~/.cache are never
	// writable by a worker, so nothing a worker stores runs unconfined later.
	env = []string{
		"CLAUDE_CODE_DISABLE_CLAUDE_MDS=1",
		"CLAUDE_CONFIG_DIR=" + g.RunDir + "/claude-config",
		"XDG_CACHE_HOME=" + g.RunDir + "/cache",
		"XDG_CONFIG_HOME=" + g.RunDir + "/config",
		// goodqa#15161 review A: git reads ~/.gitconfig even with
		// XDG_CONFIG_HOME set, and dies when the Darwin profile denies it
		// (EPERM is not "missing"). The global config is the run's own.
		"GIT_CONFIG_GLOBAL=" + g.RunDir + "/config/git/config",
		"GOCACHE=" + g.RunDir + "/cache/go-build",
		"GOMODCACHE=" + g.RunDir + "/cache/gomod",
		"GOFLAGS=-modcacherw",
		"FAB_RUN=" + g.Token,
		"FAB_MISSION=" + string(g.Mission),
		"FAB_ATTEMPT=" + string(g.Run),
		"FAB_FENCE=" + strconv.FormatInt(g.Fence, 10),
	}
	return argv, env, nil
}

// ProductionLaunch is the launch configuration fabd uses for every
// claude-code worker (cmd/fab job), shared so the AC-6 (f) live check runs
// exactly the production argv.
func ProductionLaunch(binary, schemaPath, hooksDir string) Launch {
	return Launch{
		Binary:          binary,
		SchemaPath:      schemaPath,
		HooksDir:        hooksDir,
		StrictMCP:       true,
		DisableSlash:    true,
		DisallowedTools: []string{"Agent", "Workflow", "AskUserQuestion", "WebFetch", "WebSearch"},
	}
}
