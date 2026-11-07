package sched

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"fabric/internal/domain"
	"fabric/internal/storetest"
)

// goodqa#15087 AC-2: fabd states its own path, claude-code, from its own
// identity (F1). A claude-code mission's attempt is stored with path
// claude-code and no lock path. An anthropic-api mission that reached
// runOnce past the dispatch filter is still refused at START, because the
// path fabd sends is not read from the mission: no attempt row, no launch.
func TestSchedulerPassesClaudeCodePath(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	setCap(t, st, 5)
	admitSpec(t, st, "m_pcc", domain.ProviderClaudeCode, 1)
	admitSpec(t, st, "m_papi", domain.ProviderAnthropicAPI, 2)
	ln := &fakeLauncher{st: st, block: make(chan struct{})}
	t.Cleanup(func() { close(ln.block) })
	cfg := Config{Host: "h_t2", Model: "fake-model", Effort: "n/a", Rung: "R0", TierTableSHA: "test", Profile: "P1", MaxTurns: 5,
		MaxBudgetUSD: 0.01, Wall: time.Minute, WorkRoot: t.TempDir()}
	cfg.Launch.Binary = "/bin/true"
	s := New(st, cfg, ln)

	if err := s.runOnce(ctx, "m_pcc"); err != nil {
		t.Fatal(err)
	}
	var path string
	var lockNull bool
	if err := st.Pool().QueryRow(ctx, "SELECT path, lock_path IS NULL FROM attempts WHERE mission_id = 'm_pcc'").Scan(&path, &lockNull); err != nil {
		t.Fatalf("claude-code mission: no attempt row: %v", err)
	}
	if path != string(domain.ProviderClaudeCode) || !lockNull {
		t.Fatalf("claude-code attempt stored path %q, lock_path NULL %v; want claude-code, true", path, lockNull)
	}
	var cmdPath string
	if err := st.Pool().QueryRow(ctx, `SELECT coalesce(payload->>'path', '') FROM events WHERE mission_id = 'm_pcc' AND kind = 'ATTEMPT_STARTED'`).Scan(&cmdPath); err != nil || cmdPath != string(domain.ProviderClaudeCode) {
		t.Fatalf("ATTEMPT_STARTED path %q (%v), want claude-code", cmdPath, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt32(&ln.starts) < 1 {
		if time.Now().After(deadline) {
			t.Fatal("the claude-code attempt was never launched")
		}
		time.Sleep(10 * time.Millisecond)
	}
	starts := atomic.LoadInt32(&ln.starts)

	if err := s.runOnce(ctx, "m_papi"); err != nil {
		t.Fatalf("runOnce on an anthropic-api mission: %v, want a quiet refusal", err)
	}
	var n int
	if err := st.Pool().QueryRow(ctx, "SELECT count(*) FROM attempts WHERE mission_id = 'm_papi'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("anthropic-api mission: %d attempt rows (%v), want 0", n, err)
	}
	if got := atomic.LoadInt32(&ln.starts); got != starts {
		t.Fatalf("launches %d -> %d for an anthropic-api mission, want unchanged", starts, got)
	}
}
