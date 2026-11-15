package effects

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"fabric/internal/domain"
	"fabric/internal/store"
)

// M1 ports (goodqa#15966): fencing, approval binding and the shared dispatch
// cap on the governed path, a marked database with canonical keys, so they
// run in both builds. The legacy originals keep their unmarked contract.

// m1Mission creates mission m with a started, acked attempt and returns a
// canonical governed spec for it on subject.
func m1Mission(t *testing.T, ctx context.Context, st *store.Store, m domain.MissionID, subject, body string) IntentSpec {
	t.Helper()
	if err := st.CreateMission(ctx, m, domain.ProviderClaudeCode, "GoodQA goodqa#15966", "fabric", "main"); err != nil {
		t.Fatal(err)
	}
	a := domain.AttemptID("r_" + string(m)[2:])
	must := func(key string, cmd domain.Command) store.Response {
		resp, err := st.Execute(ctx, "fabd", domain.CommandID(key+":"+string(m)), cmd)
		if err != nil {
			t.Fatalf("%s %s: %v", key, m, err)
		}
		return resp
	}
	must("admit", domain.Command{Kind: domain.CmdAdmitMission, Mission: m})
	resp := must("start", domain.Command{Kind: domain.CmdStartAttempt, Path: domain.ProviderClaudeCode, Mission: m, Attempt: a, Host: "h_t2", Role: "writer"})
	fence := resp.Events[0].Fence
	must("ack", domain.Command{Kind: domain.CmdAckStart, Mission: m, Attempt: a, Fence: fence})
	return IntentSpec{Mission: m, Attempt: a, Fence: fence, Kind: GovernedKind, Subject: subject, IdemKey: M1Key(m),
		Request: json.RawMessage(`{"ticket": "goodqa#0", "body": "` + body + `"}`)}
}

func TestM1StaleFenceAndSubjectBusy(t *testing.T) {
	ctx, st, b, fake := setup(t)
	setMarker(t, ctx, st, PolicyMarkerM1)
	s := canon("x")
	s.Fence = 0
	if _, err := b.Submit(ctx, fake, s); !errors.Is(err, domain.ErrStaleFence) {
		t.Fatalf("stale fence: want ErrStaleFence, got %v", err)
	}
	if n := effectCount(t, ctx, st); n != 0 {
		t.Fatalf("a stale-fence intent was stored: %d rows", n)
	}
	// Two missions' comments on one subject: the second cannot dispatch
	// while the first is UNKNOWN.
	fake.LoseAfter = true
	s1 := m1Mission(t, ctx, st, "m_fa", "ticket:goodqa#0", "first")
	s2 := m1Mission(t, ctx, st, "m_fb", "ticket:goodqa#0", "second")
	e1, err := b.Submit(ctx, fake, s1)
	if err != nil {
		t.Fatal(err)
	}
	if e1, _ = b.Dispatch(ctx, fake, e1.ID); e1.State != Unknown {
		t.Fatalf("want UNKNOWN, got %s", e1.State)
	}
	e2, err := b.Submit(ctx, fake, s2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Dispatch(ctx, fake, e2.ID); !errors.Is(err, ErrSubjectBusy) {
		t.Fatalf("subject busy: want ErrSubjectBusy, got %v", err)
	}
	if fake.Count() != 1 {
		t.Fatalf("remote writes = %d, want 1 (the second must not have gone out)", fake.Count())
	}
}

func TestM1ApprovalBindsRequestHash(t *testing.T) {
	ctx, st, b, fake := setup(t)
	setMarker(t, ctx, st, PolicyMarkerM1)
	s := canon("publish")
	s.NeedsApproval = true
	e, err := b.Submit(ctx, fake, s)
	if err != nil || e.State != ApprovalRequired {
		t.Fatalf("submit: %+v %v", e, err)
	}
	if _, err := b.Dispatch(ctx, fake, e.ID); !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("dispatch without approval: want ErrApprovalRequired, got %v", err)
	}
	other, _ := domain.EffectBase(GovernedKind, json.RawMessage(`{"ticket":"goodqa#0","body":"tampered"}`))
	if err := b.Approve(ctx, e.ID, other, "user"); !errors.Is(err, ErrApprovalMismatch) {
		t.Fatalf("approval with another hash: want ErrApprovalMismatch, got %v", err)
	}
	if fake.Sends() != 0 {
		t.Fatalf("%d sends before a valid approval", fake.Sends())
	}
	if err := b.Approve(ctx, e.ID, e.RequestSHA, "user"); err != nil {
		t.Fatal(err)
	}
	e, err = b.Dispatch(ctx, fake, e.ID)
	if err != nil || e.State != Confirmed || fake.Count() != 1 {
		t.Fatalf("approved dispatch: %s %v remote=%d", e.State, err, fake.Count())
	}
}

func TestM1DispatchCapSharedAcrossBrokers(t *testing.T) {
	ctx, st, b, fake := setup(t)
	setMarker(t, ctx, st, PolicyMarkerM1)
	b2 := New(st)
	n := b.DispatchLimit()
	// One governed effect per mission, so n+1 live attempts: raise the
	// admission cap of this disposable database for the test.
	var prev int
	if err := st.Pool().QueryRow(ctx, `SELECT cap FROM slots WHERE id = 1`).Scan(&prev); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool().Exec(ctx, `UPDATE slots SET cap = $1 WHERE id = 1`, n+4); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		st.Pool().Exec(context.Background(), `UPDATE slots SET cap = $1 WHERE id = 1`, prev) //nolint:errcheck
	})
	ss := slowSend{Fake: fake, entered: make(chan string, n+1), release: make(chan struct{})}
	done := make(chan error, n+1)
	for i := 0; i <= n; i++ {
		m := domain.MissionID(fmt.Sprintf("m_cap%d", i))
		e, err := b.Submit(ctx, fake, m1Mission(t, ctx, st, m, fmt.Sprintf("ticket:goodqa#%d", 100+i), "cap"))
		if err != nil {
			t.Fatal(err)
		}
		br := b
		if i%2 == 1 {
			br = b2
		}
		go func(br *Broker, id string) { _, err := br.Dispatch(ctx, ss, id); done <- err }(br, e.ID)
	}
	for i := 0; i < n; i++ {
		<-ss.entered
	}
	select {
	case id := <-ss.entered:
		t.Errorf("dispatch %s entered beyond the shared limit %d", id, n)
	case <-time.After(300 * time.Millisecond):
	}
	close(ss.release)
	for i := 0; i <= n; i++ {
		<-done
	}
	if fake.Count() != n+1 {
		t.Fatalf("remote writes %d, want %d (each governed effect once)", fake.Count(), n+1)
	}
}
