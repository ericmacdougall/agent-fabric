package sched

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"fabric/internal/domain"
)

// Red-team D1 on caea50f: a per-mission error in one sweep (here a WAKE for
// a mission whose log this build cannot read) made Tick fail and Run return,
// stopping every mission; a restart picked the same row first. Per-mission
// errors are now logged and the mission parked; the loop carries on.
func TestPerMissionErrorDoesNotStopTheLoop(t *testing.T) {
	st, ln, cfg := ownerSetup(t, "m_d1")
	ctx := context.Background()
	payload, _ := json.Marshal(domain.Event{Kind: "FUTURE_KIND", Mission: "m_d1", Seq: 2})
	for _, q := range []string{
		`INSERT INTO events (mission_id, seq, kind, actor, command_id, payload) VALUES ('m_d1', 2, 'FUTURE_KIND', 'newer', 'c_future', '` + string(payload) + `')`,
		`UPDATE missions SET seq = 2, state = jsonb_set(state, '{seq}', '2') WHERE id = 'm_d1'`,
		`INSERT INTO jobs (id, mission_id, attempt_id, fence, argv, rundir, state, ended_at) VALUES ('j_d1', 'm_d1', 'r_d1_1', 1, '[]', '/nonexistent', 'EXITED', now())`,
	} {
		if _, err := st.Pool().Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.CreateMission(ctx, "m_d1b", "GoodQA goodqa#14922", "fabric", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Execute(ctx, "chat", "admit-m_d1b", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_d1b"}); err != nil {
		t.Fatal(err)
	}
	for run := 0; run < 2; run++ { // run 1 is a restart: a fresh scheduler, the same rows
		s := New(st, cfg, ln)
		for i := 0; i < 3; i++ {
			if _, err := s.Tick(ctx); err != nil {
				t.Fatalf("run %d tick %d: a per-mission error stopped the loop: %v", run, i, err)
			}
		}
	}
	if ln.starts != 1 {
		t.Fatalf("the healthy mission was not dispatched: %d starts", ln.starts)
	}
	var woken bool
	if err := st.Pool().QueryRow(ctx, "SELECT woken FROM jobs WHERE id = 'j_d1'").Scan(&woken); err != nil || woken {
		t.Fatalf("the unreadable mission's job was marked woken (%v %v)", woken, err)
	}
	a, _ := counts(t, st, "m_d1")
	if a != 0 {
		t.Fatalf("%d attempts on the unreadable mission", a)
	}
}

// D1, the dispatch path: any runOnce error (here the worktree cannot be
// made) parks that mission; the next mission in the same Tick still runs.
func TestDispatchErrorParksOnlyThatMission(t *testing.T) {
	st, ln, cfg := ownerSetup(t, "m_bad")
	ctx := context.Background()
	if err := os.MkdirAll(filepath.Join(cfg.WorkRoot, "wt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.WorkRoot, "wt", "m_bad"), nil, 0o644); err != nil { // a file where the worktree goes
		t.Fatal(err)
	}
	if err := st.CreateMission(ctx, "m_good", "GoodQA goodqa#14922", "fabric", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Execute(ctx, "chat", "admit-m_good", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_good"}); err != nil {
		t.Fatal(err)
	}
	s := New(st, cfg, ln)
	for i := 0; i < 3; i++ {
		if _, err := s.Tick(ctx); err != nil {
			t.Fatalf("tick %d: a per-mission dispatch error stopped the loop: %v", i, err)
		}
	}
	if ln.starts != 1 {
		t.Fatalf("the healthy mission was not dispatched: %d starts", ln.starts)
	}
	// Red-team N1 on 42c7bd2: a preparation failure after START is a
	// pre-launch abandonment, not a live attempt left to expire into UNKNOWN.
	m, err := st.Mission(ctx, "m_bad")
	if err != nil {
		t.Fatal(err)
	}
	if m.LiveAttempts() != 0 || m.UnresolvedAttempts() != 0 || m.PrelaunchFailures != 1 {
		t.Fatalf("worktree failure left %d live, %d unresolved attempts, %d pre-launch failures; want 0, 0, 1",
			m.LiveAttempts(), m.UnresolvedAttempts(), m.PrelaunchFailures)
	}
}

// D1, the dispatch path with an error runOnce cannot turn into an
// abandonment: the mission is parked and the next one still runs.
func TestDispatchRawErrorParksOnlyThatMission(t *testing.T) {
	st, ln, cfg := ownerSetup(t, "m_err")
	ctx := context.Background()
	if err := st.CreateMission(ctx, "m_ok", "GoodQA goodqa#14922", "fabric", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Execute(ctx, "chat", "admit-m_ok", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_ok"}); err != nil {
		t.Fatal(err)
	}
	s := New(st, cfg, ln)
	calls := 0
	s.beforeRun = func(id domain.MissionID) error {
		if id == "m_err" {
			calls++
			return errors.New("injected per-mission fault")
		}
		return nil
	}
	for i := 0; i < 3; i++ {
		if _, err := s.Tick(ctx); err != nil {
			t.Fatalf("tick %d: a per-mission dispatch error stopped the loop: %v", i, err)
		}
	}
	if ln.starts != 1 || calls != 1 {
		t.Fatalf("%d starts, %d tries of the failing mission; want 1 and 1 (parked after the first)", ln.starts, calls)
	}
}
