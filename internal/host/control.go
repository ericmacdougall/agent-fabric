package host

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"fabric/internal/domain"
)

// describe renders an inventory compactly for control details.
func describe(inv []Process) string {
	var parts []string
	for _, p := range inv {
		c := p.Command
		if i := strings.Index(c, " FAB_RUN="); i > 0 {
			c = c[:i]
		}
		if len(c) > 40 {
			c = c[:40]
		}
		parts = append(parts, fmt.Sprintf("%d/pg%d:%q", p.PID, p.PGID, c))
	}
	return strings.Join(parts, " ")
}

// controlGrant is the grant the negative controls run under: a scratch
// worktree and run dir, a placeholder model (the controls never launch a
// provider), and a fresh token.
func controlGrant(scratch string) Grant {
	wt := filepath.Join(scratch, "wt")
	rd := filepath.Join(scratch, "run")
	os.MkdirAll(wt, 0o755) //nolint:errcheck
	os.MkdirAll(rd, 0o755) //nolint:errcheck
	return Grant{Run: domain.AttemptID("r_nc_" + strconv.FormatInt(time.Now().UnixNano(), 36)), Mission: "m_nc", Fence: 1, Model: "control", Effort: "n/a", Rung: "R0",
		TierTableSHA: "control", Profile: "P1", MaxTurns: 1, MaxBudgetUSD: 0.01, Wall: time.Minute,
		Worktree: wt, RunDir: rd, Token: "nc-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "-0123456789abcdef"}
}
