package sched

import (
	"context"
	"errors"
	"testing"
	"time"

	"fabric/internal/domain"
	"fabric/internal/store"
)

// Red-team G1 on dcd2eb7: nothing resolved an UNKNOWN attempt, so with the
// gate on a scheduler restart mid-run pinned the runner's capacity forever
// and the waiting mission had no signal. Three parts: the owner keeps the
// lease alive while its runner runs; a later scheduler reads the runner
// back and quarantines the attempt once the runner is provably gone; and a
// capacity wait is counted and names its holder.

type fakeProber struct{ alive, known, jobAlive bool }

func (p *fakeProber) Alive(domain.AttemptID) (bool, bool) { return p.alive, p.known }
func (p *fakeProber) JobAlive(string) (bool, bool)        { return p.jobAlive, p.known }

func waitLeaseLapse(t *testing.T, st *store.Store, attempt string) {
	t.Helper()
	for i := 0; i < 400; i++ {
		var past bool
		if err := st.Pool().QueryRow(context.Background(), "SELECT lease_until < now() FROM attempts WHERE id = $1", attempt).Scan(&past); err != nil {
			t.Fatal(err)
		}
		if past {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("lease never lapsed")
}

// The owner heartbeats: a run longer than the lease stays RUNNING.
func TestOwnerHeartbeatsLiveRun(t *testing.T) {
	st, ln, cfg := ownerSetup(t, "m_hb")
	st.Lease = 600 * time.Millisecond
	s := New(st, cfg, ln)
	if err := s.runOnce(context.Background(), "m_hb"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(4 * st.Lease)
	if _, err := st.ExpireLeases(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	if got := attemptState(t, st, "r_hb_1"); got != string(domain.AttemptRunning) {
		t.Fatalf("a live run outlasting its lease went %s; the owner must heartbeat", got)
	}
}

func crashMidRun(t *testing.T, p *fakeProber) (*store.Store, *fakeLauncher, *Scheduler) {
	t.Helper()
	st, ln, cfg := gatedSetup(t, "m_ga", 1)
	st.Lease = 500 * time.Millisecond
	ctx := context.Background()
	if err := st.CreateMission(ctx, "m_gb", domain.ProviderClaudeCode, "GoodQA goodqa#14922", "fabric", "main"); err != nil {
		t.Fatal(err)
	}
	// The first scheduler starts m_ga's runner, then dies: its context ends,
	// so it heartbeats no more, and its runner is never settled.
	runCtx, crash := context.WithCancel(ctx)
	a := New(st, cfg, ln)
	if err := a.runOnce(runCtx, "m_ga"); err != nil {
		t.Fatal(err)
	}
	crash()
	if _, err := st.Execute(ctx, "chat", "admit-m_gb", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_gb"}); err != nil {
		t.Fatal(err)
	}
	waitLeaseLapse(t, st, "r_ga_1")
	b := New(st, cfg, ln)
	b.prober = p
	for i := 0; i < 5; i++ {
		if _, err := b.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	return st, ln, b
}

// The restarted scheduler finds the runner gone: the attempt is
// quarantined (it ran; its outcome is unverified), the slot frees and the
// waiting mission runs.
func TestRestartReconcilesGoneRunner(t *testing.T) {
	st, ln, _ := crashMidRun(t, &fakeProber{alive: false, known: true})
	if got := attemptState(t, st, "r_ga_1"); got != string(domain.AttemptQuarantined) {
		t.Fatalf("the dead runner's attempt is %s, want QUARANTINED", got)
	}
	if ln.starts != 2 {
		t.Fatalf("%d starts; want 2 (m_ga before the crash, m_gb after the slot freed)", ln.starts)
	}
}

// A runner that is still alive keeps its slot; the waiting mission is
// counted as waiting and starts nothing.
func TestRestartKeepsLiveRunnerSlot(t *testing.T) {
	st, ln, b := crashMidRun(t, &fakeProber{alive: true, known: true})
	if got := attemptState(t, st, "r_ga_1"); got != string(domain.AttemptUnknown) {
		t.Fatalf("the live runner's attempt is %s, want UNKNOWN", got)
	}
	if a, _ := counts(t, st, "m_gb"); a != 0 || ln.starts != 1 {
		t.Fatalf("m_gb has %d attempts, %d starts in all; want 0 and 1", a, ln.starts)
	}
	if b.CapacityWaits == 0 {
		t.Fatal("the capacity wait left no signal")
	}
	var holder string
	if err := st.Pool().QueryRow(context.Background(), "SELECT value::text FROM sched_counters WHERE name = 'capacity_waits_total'").Scan(&holder); err != nil {
		t.Fatalf("capacity_waits_total not recorded: %v", err)
	}
}

// No readback answer is never "gone": the attempt stays UNKNOWN.
func TestRestartWithoutReadbackKeepsUnknown(t *testing.T) {
	st, ln, _ := crashMidRun(t, &fakeProber{known: false})
	if got := attemptState(t, st, "r_ga_1"); got != string(domain.AttemptUnknown) || ln.starts != 1 {
		t.Fatalf("with no readback the attempt is %s and there were %d starts; want UNKNOWN and 1", got, ln.starts)
	}
}

// Red-team H2 on 193c0a1: one transient heartbeat error stopped the
// heartbeat, so a healthy run could lapse into UNKNOWN and be quarantined.
func TestHeartbeatSurvivesTransientError(t *testing.T) {
	st, ln, cfg := ownerSetup(t, "m_hbt")
	st.Lease = 600 * time.Millisecond
	s := New(st, cfg, ln)
	failed := false
	s.beforeHeartbeat = func() error {
		if !failed {
			failed = true
			return errors.New("injected transient fault")
		}
		return nil
	}
	if err := s.runOnce(context.Background(), "m_hbt"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(4 * st.Lease)
	if _, err := st.ExpireLeases(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	if got := attemptState(t, st, "r_hbt_1"); got != string(domain.AttemptRunning) || !failed {
		t.Fatalf("after one transient heartbeat error the run went %s (fault injected: %v)", got, failed)
	}
}

// Main's follow-up to H1: a runner may record its job's exit, but a forged
// EXITED for a job whose unit still runs must not wake the mission.
func TestForgedJobExitDoesNotWake(t *testing.T) {
	st, ln, cfg := ownerSetup(t, "m_fj")
	ctx := context.Background()
	if _, err := st.Pool().Exec(ctx, `INSERT INTO jobs (id, mission_id, attempt_id, fence, argv, rundir, state, ended_at)
		VALUES ('j_fj', 'm_fj', 'r_fj_1', 1, '[]', '/x', 'EXITED', now())`); err != nil {
		t.Fatal(err)
	}
	woken := func() bool {
		var w bool
		if err := st.Pool().QueryRow(ctx, "SELECT woken FROM jobs WHERE id = 'j_fj'").Scan(&w); err != nil {
			t.Fatal(err)
		}
		return w
	}
	s := New(st, cfg, ln)
	p := &fakeProber{known: true, jobAlive: true}
	s.SetProber(p)
	if _, err := s.wakeExitedJobs(ctx); err != nil {
		t.Fatal(err)
	}
	if woken() {
		t.Fatal("a job reported EXITED while its unit is alive woke the mission")
	}
	p.jobAlive = false
	s.parked = nil // the live unit parked nothing; clear any park so the retry runs now
	if _, err := s.wakeExitedJobs(ctx); err != nil {
		t.Fatal(err)
	}
	if !woken() {
		t.Fatal("control: the job's unit is gone, yet it was not woken")
	}
}
