package native

import (
	"context"
	"encoding/json"
	"regexp"
	"time"

	"fabric/internal/agent"
	"fabric/internal/domain"
	"fabric/internal/host"
	"fabric/internal/policy"
	"fabric/internal/settle"
	"fabric/internal/store"
	"fabric/internal/tools"
)

// SetFinishSeams installs the finish-retry test seams and returns their
// restore (goodqa#15087 AC-7).
func SetFinishSeams(after func(store.Response, error) error, clock settle.Clock) func() {
	prevAfter, prevClock := afterFinishExec, settleClock
	afterFinishExec, settleClock = after, clock
	return func() { afterFinishExec, settleClock = prevAfter, prevClock }
}

// NewKeyRing exposes the native key source and its redactor (goodqa#15087
// AC-5).
func NewKeyRing(argv []string, ttl time.Duration) (func(context.Context) (string, error), interface{ Redact(string) string }, error) {
	src, ring, err := newKeyRing(argv, ttl, "", nil)
	if err != nil {
		return nil, nil, err
	}
	return src, ring, nil
}

// DefaultMonthSource is the production month source (goodqa#15124).
var DefaultMonthSource = monthSource

// SetMonthSource replaces the month cap's month source and returns the
// restore (goodqa#15124).
func SetMonthSource(f func(context.Context, *store.Store) (string, error)) func() {
	prev := monthSource
	monthSource = f
	return func() { monthSource = prev }
}

// StartNativeForTest exposes the keyed start/ack (goodqa#15235 S4).
// The attempt is bound to the canonical form of g.Worktree (goodqa#15283).
func StartNativeForTest(ctx context.Context, st *store.Store, g Grant, admit bool, lockPath string) (int64, error) {
	wt, err := canonicalWorktree(g.Worktree)
	if err != nil {
		return 0, err
	}
	return startNative(ctx, st, g, admit, lockPath, wt.Path())
}

// NativeToolsForTest builds the oracle and the toolset exactly as Run does
// for g, on its canonical, pinned worktree, and returns that path
// (goodqa#15283 AC-5).
func NativeToolsForTest(g Grant, starter tools.Starter) (*policy.Oracle, []agent.Tool, string, error) {
	wt, err := canonicalWorktree(g.Worktree)
	if err != nil {
		return nil, nil, "", err
	}
	var allow []*regexp.Regexp
	for _, pat := range g.ShellAllow {
		allow = append(allow, regexp.MustCompile(pat))
	}
	// The caller's Worktree in the host grant is overridden by nativeToolset.
	hg := host.Grant{Run: domain.AttemptID(g.Attempt), Mission: domain.MissionID(g.Mission), Fence: 1, Model: g.Model, Effort: "n/a",
		Rung: "native", TierTableSHA: "native", Profile: "P1", MaxTurns: 1, MaxBudgetUSD: 1, Wall: time.Minute,
		Worktree: g.Worktree, RunDir: g.RunDir, Token: "test-token-0123456789abcdef"}
	ts, err := nativeToolset(wt, starter, hg, false, allow)
	return nativeOracle(g, wt), ts, wt.Path(), err
}

// DisablePrecheck turns off native's own provider checks and returns the
// restore (goodqa#15087 AC-2 defense in depth).
func DisablePrecheck() func() {
	skipPrecheck = true
	return func() { skipPrecheck = false }
}

// SetBeforeLocks installs the red-team K3 seam and returns its restore.
func SetBeforeLocks(f func()) func() {
	prev := beforeLocks
	beforeLocks = f
	return func() { beforeLocks = prev }
}

// SetHeartbeatFault installs the heartbeat test seam and returns its
// restore (goodqa#15087 AC-7).
func SetHeartbeatFault(f func() error) func() {
	prev := heartbeatFault
	heartbeatFault = f
	return func() { heartbeatFault = prev }
}

// FileToolsEnabled exposes the edit/write gate rule (goodqa#15361 F1).
func FileToolsEnabled(reviewGate bool, c ReviewGateConfig) bool {
	return fileToolsEnabled(reviewGate, c)
}

// ToolNamesForTest builds the toolset as Run does, with or without edit and
// write and a state directory, and returns the tool names.
func ToolNamesForTest(g Grant, starter tools.Starter, fileTools bool, stateDir string) ([]string, error) {
	wt, err := canonicalWorktree(g.Worktree)
	if err != nil {
		return nil, err
	}
	hg := host.Grant{Run: domain.AttemptID(g.Attempt), Mission: domain.MissionID(g.Mission), Fence: 1, Model: g.Model, Effort: "n/a",
		Rung: "native", TierTableSHA: "native", Profile: "P1", MaxTurns: 1, MaxBudgetUSD: 1, Wall: time.Minute,
		Worktree: g.Worktree, RunDir: g.RunDir, Token: "test-token-0123456789abcdef"}
	ts, res, err := nativeToolsetOpts(wt, starter, hg, false, nil, toolsetOpts{FileTools: fileTools, StateDir: stateDir, Attempt: g.Attempt})
	if err != nil {
		return nil, err
	}
	defer res.close()
	var names []string
	for _, t := range ts {
		names = append(names, t.Name())
	}
	return names, nil
}

// PolicyFor exposes the per-call review policy of a grant's gate config.
func PolicyFor(c ReviewGateConfig) (map[string]bool, error) {
	p, err := c.policy()
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for name, tp := range p.Tools {
		out[name] = tp.HumanRequired
	}
	return out, nil
}

// CompactionExtraForTest exposes the request parameters mode S adds.
func CompactionExtraForTest(g Grant) (map[string]json.RawMessage, []string, error) {
	return compactionExtra(g)
}

// GrantPinForTest exposes the grant's resume pin.
func GrantPinForTest(g Grant, reviewGate bool) string { return g.pin(reviewGate) }

// AllowStateDirUnderForTest exempts state directories under prefix from the
// temp-root clause of the C5 check for the duration of the test. It exists
// only in test binaries; the production check has no override. The worktree,
// run directory and extra-writable clauses still apply.
func AllowStateDirUnderForTest(t interface {
	Helper()
	Cleanup(func())
}, prefix string) {
	t.Helper()
	prev := stateDirTestExempt
	stateDirTestExempt = prefix
	t.Cleanup(func() { stateDirTestExempt = prev })
}

// CheckStateDirForTest exposes the C5 rule exactly as nativeToolsetOpts applies it.
func CheckStateDirForTest(stateDir, worktree, runDir string, extra []string) error {
	return checkStateDir(stateDir, worktree, runDir, extra)
}
