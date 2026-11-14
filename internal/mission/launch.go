package mission

import (
	"fmt"

	"fabric/internal/host"
	"fabric/internal/provider/claude"
)

// ApplyLaunch pins the tool surface of an M1 launch (AC-24): --tools
// Read,Grep,Glob,Bash, the attempt's settings file (permission rules and
// the Bash gate), the brief through --append-system-prompt-file, and the
// dontAsk mode. Whatever the base launch said about these is replaced.
func (c *Config) ApplyLaunch(l claude.Launch, settingsPath, hooksDir string) claude.Launch {
	l.Tools = c.Tools()
	l.SettingsPath = settingsPath
	l.HooksDir = hooksDir
	l.BriefPath = c.brief
	l.PermissionMode = PermissionMode
	return l
}

// ApplyGrant sets the grant's caps from the config (AC-25) and the
// worktree. The M1 path runs only the P0 (read-only worktree) profile: P1
// makes the worktree writable in the sandbox (goodqa#15976 note).
func (c *Config) ApplyGrant(g *host.Grant, worktree string) error {
	if g.Profile != "P0" {
		return fmt.Errorf("%w: the M1 path runs only profile P0, not %q", ErrConfig, g.Profile)
	}
	if err := CheckCaps(c.caps); err != nil {
		return err
	}
	g.MaxTurns = c.caps.MaxTurns
	g.Wall = c.caps.Wall
	g.MaxBudgetUSD = c.caps.MaxBudgetUSD
	g.Worktree = worktree
	return nil
}
