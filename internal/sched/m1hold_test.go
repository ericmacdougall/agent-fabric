package sched

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"fabric/internal/credential"
	"fabric/internal/domain"
	"fabric/internal/records"
	"fabric/internal/store"
)

// Owner rulings C and R1, through the store: a fail-closed settle finishes
// the attempt abandoned with the m1 reason and places the operator hold.
// The hold survives 10 ticks (the undecodable-log drain runs and releases
// its own row, never this one) and a fabd restart; no attempt starts while
// it is held; once released, the mission dispatches again, so the refusal
// is the hold's and not a terminal mission.
func TestM1FailClosedHoldsMission(t *testing.T) {
	ctx := context.Background()
	m, a := domain.MissionID("m_m1hold"), domain.AttemptID("r_m1hold_1")
	st, ln, cfg := ownerSetup(t, m)
	t.Cleanup(func() { close(ln.block) })
	f := newM1FixtureFor(t, true, m, a)
	resp, err := st.Execute(ctx, "fabd", domain.CommandID("start:"+string(a)), domain.Command{Kind: domain.CmdStartAttempt, Path: domain.ProviderClaudeCode, Mission: m, Attempt: a, Host: cfg.Host, Role: "writer"})
	if err != nil {
		t.Fatal(err)
	}
	fence := resp.Events[0].Fence
	if _, err := st.Execute(ctx, "fabd", domain.CommandID("ack:"+string(a)), domain.Command{Kind: domain.CmdAckStart, Mission: m, Attempt: a, Fence: fence}); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(f.bindings, string(a)+".json"))
	cfg.Credential, cfg.RecordsDir, cfg.Sweep = f.s.cfg.Credential, f.recDir, f.s.cfg.Sweep
	s := New(st, cfg, ln)
	if err := s.afterWaitRun(ctx, m, a, fence, f.sdir, f.run, 0, nil, verified); err != nil {
		t.Fatal(err)
	}
	var reason string
	st.Pool().QueryRow(ctx, `SELECT payload->>'reason' FROM events WHERE mission_id = $1 AND kind = 'ATTEMPT_FINISHED'`, string(m)).Scan(&reason) //nolint:errcheck
	if reason != "m1:delivery_blocked:"+credential.ClassMaterialUnavailable {
		t.Fatalf("finish reason %q", reason)
	}
	count := func(prefix string) int {
		var n int
		st.Pool().QueryRow(ctx, `SELECT count(*) FROM outbox_cursors WHERE consumer = $1 || $2`, prefix, string(m)).Scan(&n) //nolint:errcheck
		return n
	}
	if count(store.OpHoldPrefix) != 1 {
		t.Fatal("no operator hold")
	}
	hold := f.kinds(t)[records.MissionHold]
	if hold != credential.ClassMaterialUnavailable {
		t.Fatalf("hold record class %q", hold)
	}
	noNewAttempt := func(when string) {
		t.Helper()
		if n := atomic.LoadInt32(&ln.starts); n != 0 {
			t.Fatalf("%s: %d starts while the mission is held", when, n)
		}
		var others int
		st.Pool().QueryRow(ctx, `SELECT count(*) FROM attempts WHERE mission_id = $1 AND id <> $2`, string(m), string(a)).Scan(&others) //nolint:errcheck
		if others != 0 {
			t.Fatalf("%s: %d new attempts while the mission is held", when, others)
		}
	}
	for i := 0; i < 10; i++ {
		if _, err := s.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	noNewAttempt("10 ticks")
	if count(store.HoldPrefix) != 0 {
		t.Fatal("the undecodable-log hold was not drained: the drain path never ran, so the survival check is vacuous")
	}
	if count(store.OpHoldPrefix) != 1 {
		t.Fatal("the drain removed the operator hold")
	}
	// The pre-START check alone refuses too (no START, no event).
	if err := s.runOnce(ctx, m); err != nil {
		t.Fatal(err)
	}
	noNewAttempt("direct runOnce")
	// A fabd restart: a new scheduler over the same store.
	s2 := New(st, cfg, ln)
	for i := 0; i < 3; i++ {
		if _, err := s2.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	noNewAttempt("after restart")
	if released, err := st.ReleaseOperatorHold(ctx, m, func(int64) error { return nil }); err != nil || !released {
		t.Fatalf("release: %v %v", released, err)
	}
	s2.cfg.Credential = nil // the released mission dispatches on the plain path here
	for i := 0; i < 5 && atomic.LoadInt32(&ln.starts) == 0; i++ {
		if _, err := s2.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if atomic.LoadInt32(&ln.starts) == 0 {
		t.Fatal("released mission never dispatched: the held checks above are vacuous")
	}
}

// AC-34: dispatch selection (the scheduler's and fab job
// dispatch-candidates') leaves an operator-held mission out, and includes it
// again once released.
func TestDispatchCandidatesSkipOperatorHold(t *testing.T) {
	st, _, _ := registrySetup(t, "m_oph0")
	admitSpec(t, st, "m_oph1", domain.ProviderClaudeCode, 1)
	ctx := context.Background()
	if err := st.OperatorHold(ctx, "m_oph1"); err != nil {
		t.Fatal(err)
	}
	got, err := DispatchCandidates(ctx, st.Pool(), nil, "")
	if err != nil || fmt.Sprint(got) != "[m_oph0]" {
		t.Fatalf("held candidates %v %v, want [m_oph0]", got, err)
	}
	if got, _ := DispatchCandidates(ctx, st.Pool(), nil, "m_oph1"); len(got) != 0 {
		t.Fatalf("--only-mission selected a held mission: %v", got)
	}
	if _, err := st.ReleaseOperatorHold(ctx, "m_oph1", func(int64) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if got, err := DispatchCandidates(ctx, st.Pool(), nil, ""); err != nil || fmt.Sprint(got) != "[m_oph0 m_oph1]" {
		t.Fatalf("released candidates %v %v", got, err)
	}
}

// heldAttempt is a leased, acknowledged credential attempt whose binding is
// gone, so its settle fails closed (credential_material_unavailable) and
// finishM1 must place the operator hold. The store lease is short so the
// lease-expiry path can be observed.
func heldAttempt(t *testing.T, m domain.MissionID, a domain.AttemptID) (*store.Store, *fakeLauncher, Config, *m1Fixture, int64) {
	t.Helper()
	ctx := context.Background()
	st, ln, cfg := ownerSetup(t, m)
	t.Cleanup(func() { close(ln.block) })
	st.Lease = 600 * time.Millisecond
	f := newM1FixtureFor(t, true, m, a)
	resp, err := st.Execute(ctx, "fabd", domain.CommandID("start:"+string(a)), domain.Command{Kind: domain.CmdStartAttempt, Path: domain.ProviderClaudeCode, Mission: m, Attempt: a, Host: cfg.Host, Role: "writer"})
	if err != nil {
		t.Fatal(err)
	}
	fence := resp.Events[0].Fence
	if _, err := st.Execute(ctx, "fabd", domain.CommandID("ack:"+string(a)), domain.Command{Kind: domain.CmdAckStart, Mission: m, Attempt: a, Fence: fence}); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(f.bindings, string(a)+".json"))
	cfg.Credential, cfg.RecordsDir, cfg.Sweep = f.s.cfg.Credential, f.recDir, f.s.cfg.Sweep
	return st, ln, cfg, f, fence
}

func m1AttemptState(t *testing.T, st *store.Store, a domain.AttemptID) (state string, finished int) {
	t.Helper()
	ctx := context.Background()
	if err := st.Pool().QueryRow(ctx, `SELECT state FROM attempts WHERE id = $1`, string(a)).Scan(&state); err != nil {
		t.Fatal(err)
	}
	st.Pool().QueryRow(ctx, `SELECT count(*) FROM events WHERE mission_id = (SELECT mission_id FROM attempts WHERE id = $1) AND kind = 'ATTEMPT_FINISHED'`, string(a)).Scan(&finished) //nolint:errcheck
	return state, finished
}

// tickPastLease ticks until the attempt's lease has lapsed and the sweep ran.
func tickPastLease(t *testing.T, s *Scheduler, st *store.Store) {
	t.Helper()
	time.Sleep(st.Lease + 200*time.Millisecond)
	for i := 0; i < 3; i++ {
		if _, err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

func assertNoStart(t *testing.T, st *store.Store, ln *fakeLauncher, m domain.MissionID, a domain.AttemptID) {
	t.Helper()
	if n := atomic.LoadInt32(&ln.starts); n != 0 {
		t.Fatalf("%d starts", n)
	}
	var others int
	st.Pool().QueryRow(context.Background(), `SELECT count(*) FROM attempts WHERE mission_id = $1 AND id <> $2`, string(m), string(a)).Scan(&others) //nolint:errcheck
	if others != 0 {
		t.Fatalf("%d new attempts", others)
	}
}

// AC-34, red-team H-1: when the operator hold cannot be written the attempt
// is not finished. It stays live, its lease lapses into UNKNOWN (the
// reconcile path), and nothing starts. Under the old order (FINISH, then
// the hold) the attempt is finished and the mission is left unheld.
func TestM1OpHoldWriteFailureLeavesAttemptLeased(t *testing.T) {
	m, a := domain.MissionID("m_m1hf"), domain.AttemptID("r_m1hf_1")
	st, ln, cfg, f, fence := heldAttempt(t, m, a)
	s := New(st, cfg, ln)
	s.settleFault = func(step string) error {
		if step == stepOpHold {
			return errors.New("injected hold write failure")
		}
		return nil
	}
	if err := s.afterWaitRun(context.Background(), m, a, fence, f.sdir, f.run, 0, nil, verified); err == nil {
		t.Fatal("a failed hold write was not reported")
	}
	if state, fin := m1AttemptState(t, st, a); fin != 0 || state == "SUCCEEDED" || state == "ABANDONED" || state == "FAILED" {
		t.Fatalf("attempt finished without its hold: state %s, %d finish events", state, fin)
	}
	if held, _ := st.OperatorHeld(context.Background(), m); held {
		t.Fatal("fixture: the injected failure still wrote the hold")
	}
	s.settleFault = nil
	tickPastLease(t, s, st)
	if s.Expired < 1 {
		t.Fatal("the lease never expired")
	}
	if state, _ := m1AttemptState(t, st, a); state != "UNKNOWN" {
		t.Fatalf("after lease expiry the attempt is %s, want UNKNOWN for reconcile", state)
	}
	assertNoStart(t, st, ln, m, a)
}

// AC-34, red-team H-1: a crash between the hold write and FINISH leaves the
// hold in place. After a restart nothing starts until release, and the
// operator hold does not exempt the live attempt from lease expiry into
// reconcile (the ophold.go contract). Under the old order the crash point
// is after FINISH and before the hold: the mission is finished and unheld.
func TestM1CrashBetweenHoldAndFinish(t *testing.T) {
	ctx := context.Background()
	m, a := domain.MissionID("m_m1hc"), domain.AttemptID("r_m1hc_1")
	st, ln, cfg, f, fence := heldAttempt(t, m, a)
	s := New(st, cfg, ln)
	s.settleFault = func(step string) error {
		if step == stepFinish {
			return errors.New("crash")
		}
		return nil
	}
	s.afterWaitRun(ctx, m, a, fence, f.sdir, f.run, 0, nil, verified) //nolint:errcheck // the crash
	if held, err := st.OperatorHeld(ctx, m); err != nil || !held {
		t.Fatalf("no operator hold at the crash point: %v %v", held, err)
	}
	if _, fin := m1AttemptState(t, st, a); fin != 0 {
		t.Fatal("FINISH ran before the hold was written")
	}
	s2 := New(st, cfg, ln) // fabd restarts
	tickPastLease(t, s2, st)
	if s2.Expired < 1 {
		t.Fatal("the operator hold exempted the attempt from lease expiry")
	}
	if state, _ := m1AttemptState(t, st, a); state != "UNKNOWN" {
		t.Fatalf("after lease expiry the attempt is %s, want UNKNOWN for reconcile", state)
	}
	assertNoStart(t, st, ln, m, a)
	if held, _ := st.OperatorHeld(ctx, m); !held {
		t.Fatal("the hold was lost across the restart")
	}
}
