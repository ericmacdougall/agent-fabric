package host

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"fabric/internal/domain"
)

var controlRunSeq atomic.Int64

// freshRun returns g with its own run id. Each control start needs one: a run
// id names the unit and the environment file, and a control that reuses its
// predecessor's id collides with the file or unit the predecessor left behind
// (goodqa#15466: the worker-account controls failed with "file exists").
func freshRun(g Grant) Grant {
	g.Run = domain.AttemptID("r_nc_" + strconv.FormatInt(time.Now().UnixNano(), 36) + "_" + strconv.FormatInt(controlRunSeq.Add(1), 36))
	return g
}

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
	if root := os.Getenv("FABRIC_WORKER_ROOT"); root != "" {
		scratch = filepath.Join(root, "nc-"+strconv.FormatInt(time.Now().UnixNano(), 36)) // shared with the worker account
	}
	wt := filepath.Join(scratch, "wt")
	rd := filepath.Join(scratch, "run")
	os.MkdirAll(wt, 0o775)            //nolint:errcheck
	os.MkdirAll(rd, 0o775)            //nolint:errcheck
	os.Chmod(wt, 0o775|os.ModeSetgid) //nolint:errcheck
	os.Chmod(rd, 0o775|os.ModeSetgid) //nolint:errcheck
	return Grant{Run: domain.AttemptID("r_nc_" + strconv.FormatInt(time.Now().UnixNano(), 36)), Mission: "m_nc", Fence: 1, Model: "control", Effort: "n/a", Rung: "R0",
		TierTableSHA: "control", Profile: "P1", MaxTurns: 1, MaxBudgetUSD: 0.01, Wall: time.Minute,
		Worktree: wt, RunDir: rd, Token: "nc-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "-0123456789abcdef"}
}

// Capabilities summarises what the controls demonstrated: the daemon refuses
// untrusted workers unless every containment control passed.
func Capabilities(results []ControlResult) (containment bool, failed []string) {
	containment = true
	for _, r := range results {
		if !r.Passed {
			containment = false
			failed = append(failed, r.Name)
		}
	}
	return containment, failed
}
