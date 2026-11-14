package sched

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fabric/internal/domain"
)

// goodqa#15966 AC-7, AC-29: the EffectStep runs after the stream settled,
// while the attempt is live (its heartbeat succeeds), before FINISH_ATTEMPT,
// and its prefix lands on the finish reason. EffectPass runs on every tick,
// with NoDispatch too.
func TestEffectStepBeforeFinish(t *testing.T) {
	m := domain.MissionID("m_effstep")
	a := domain.AttemptID("r_effstep_1")
	st, ln, cfg := ownerSetup(t, m)
	ctx := context.Background()
	resp, err := st.Execute(ctx, "fabd", domain.CommandID("start:"+string(a)), domain.Command{Kind: domain.CmdStartAttempt, Path: domain.ProviderClaudeCode, Mission: m, Attempt: a, Host: cfg.Host, Role: "writer"})
	if err != nil {
		t.Fatal(err)
	}
	fence := resp.Events[0].Fence
	if _, err := st.Execute(ctx, "fabd", domain.CommandID("ack:"+string(a)), domain.Command{Kind: domain.CmdAckStart, Mission: m, Attempt: a, Fence: fence}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "stream.jsonl"), []byte(initEv+"\n"+okResult+"\n"), 0o600) //nolint:errcheck
	var seen EffectAttempt
	var liveAtStep string
	cfg.EffectStep = func(ctx context.Context, ea EffectAttempt, hb func(context.Context) error) string {
		seen = ea
		if err := hb(ctx); err != nil {
			t.Errorf("heartbeat at the effect step: %v", err)
		}
		st.Pool().QueryRow(ctx, "SELECT state FROM attempts WHERE id = $1", string(a)).Scan(&liveAtStep) //nolint:errcheck
		return "m1:delivery_blocked:lease_budget"
	}
	s := New(st, cfg, ln)
	if err := s.afterWait(ctx, m, a, fence, dir, 0, nil); err != nil {
		t.Fatal(err)
	}
	if seen.Attempt != a || seen.Outcome != "checkpoint" || seen.Fence != fence {
		t.Fatalf("step saw %+v", seen)
	}
	if !domain.AttemptState(liveAtStep).Live() {
		t.Fatalf("attempt was %s at the effect step, want live", liveAtStep)
	}
	var reason string
	st.Pool().QueryRow(ctx, `SELECT payload->>'reason' FROM events WHERE mission_id = $1 AND kind = 'ATTEMPT_FINISHED'`, string(m)).Scan(&reason) //nolint:errcheck
	if !strings.HasPrefix(reason, "m1:delivery_blocked:lease_budget") {
		t.Fatalf("finish reason %q", reason)
	}

	passes := 0
	st2, ln2, cfg2 := ownerSetup(t, "m_effpass")
	cfg2.NoDispatch = true
	cfg2.EffectPass = func(context.Context) { passes++ }
	s2 := New(st2, cfg2, ln2)
	for i := 0; i < 3; i++ {
		if _, err := s2.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if passes != 3 {
		t.Fatalf("EffectPass ran %d times in 3 ticks", passes)
	}
}
