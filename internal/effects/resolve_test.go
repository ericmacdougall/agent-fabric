package effects

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"fabric/internal/domain"
	"fabric/internal/store"
)

// appBroker opens the broker as the app role (fabd, fab, an operator).
func appBroker(t *testing.T, ctx context.Context, st *store.Store) *Broker {
	t.Helper()
	r := store.Roles{Prefix: fmt.Sprintf("fabric_e%d", os.Getpid())}
	u, err := url.Parse(testDSN)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("role", r.App())
	u.RawQuery = q.Encode()
	as, err := store.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(as.Close)
	return New(as)
}

// Review N2 on aecf3fa (main ruling): a QUARANTINED effect has an
// operator exit to RESOLVED_APPLIED or RESOLVED_NOT_APPLIED. App role only,
// a reason and an evidence ref required, audited, never the runner.
func TestResolveQuarantinedEffect(t *testing.T) {
	ctx, st, _, fake := setup(t)
	ackRun(t, ctx, st)
	_, rb := runnerBroker(t, ctx, st)
	ab := appBroker(t, ctx, st)
	e, err := rb.Submit(ctx, fake, spec("kq1", "conflicted"))
	if err != nil {
		t.Fatal(err)
	}
	// Fixture: the broker's path to QUARANTINED (a readback conflict).
	for _, q := range []string{
		"UPDATE effects SET state = 'DISPATCHED', dispatches = dispatches + 1 WHERE id = $1",
		"UPDATE effects SET state = 'UNKNOWN' WHERE id = $1",
		"UPDATE effects SET state = 'RECONCILING' WHERE id = $1",
		"UPDATE effects SET state = 'QUARANTINED' WHERE id = $1",
	} {
		if _, err := st.Pool().Exec(ctx, q, e.ID); err != nil {
			t.Fatalf("fixture %q: %v", q, err)
		}
	}
	if _, err := rb.ResolveQuarantined(ctx, e.ID, ResolvedNotApplied, "read back", "rcpt_rb", "runner"); code(err) != "42501" {
		t.Errorf("runner resolved a quarantined effect: %v, want 42501", err)
	}
	for _, c := range []struct {
		name               string
		outcome            State
		reason, evid, whom string
	}{
		{"no reason", ResolvedNotApplied, "", "rcpt", "eric"},
		{"no evidence", ResolvedNotApplied, "read back", "", "eric"},
		{"no actor", ResolvedNotApplied, "read back", "rcpt", ""},
		{"not an operator outcome", Confirmed, "read back", "rcpt", "eric"},
	} {
		if _, err := ab.ResolveQuarantined(ctx, e.ID, c.outcome, c.reason, c.evid, c.whom); !errors.Is(err, domain.ErrInvalidTransition) {
			t.Errorf("%s: %v, want ErrInvalidTransition", c.name, err)
		}
	}
	// A raw state write without an audit row is refused, even for the app role.
	if _, err := ab.pool.Exec(ctx, "UPDATE effects SET state = 'RESOLVED_APPLIED' WHERE id = $1", e.ID); err == nil {
		t.Error("an unaudited RESOLVED_APPLIED write was accepted")
	}
	// Nor can a row be born resolved.
	for _, st := range []string{"RESOLVED_APPLIED", "RESOLVED_NOT_APPLIED"} {
		if _, err := ab.pool.Exec(ctx, `INSERT INTO effects (id, mission_id, attempt_id, fence, kind, subject, idem_key, request, request_sha, state)
			VALUES ('e_born', 'm_1', 'r_1', 1, 'fake', 's_born', 'k_born', '{}', 'sha', $1)`, st); err == nil {
			t.Errorf("an effect was inserted already %s", st)
		}
	}
	got, err := ab.ResolveQuarantined(ctx, e.ID, ResolvedNotApplied, "ticket comments read back: not posted", "wal:w_x", "eric")
	if err != nil || got.State != ResolvedNotApplied {
		t.Fatalf("operator resolve: %+v %v", got, err)
	}
	var n int
	var actor, reason, evid, from string
	if err := st.Pool().QueryRow(ctx, `SELECT count(*) OVER (), actor, reason, evidence, from_state FROM effect_resolutions WHERE effect_id = $1`, e.ID).
		Scan(&n, &actor, &reason, &evid, &from); err != nil || n != 1 || actor != "eric" || evid != "wal:w_x" || from != "QUARANTINED" {
		t.Fatalf("audit row: n=%d actor=%q evidence=%q from=%q err=%v", n, actor, evid, from, err)
	}
	if _, err := ab.ResolveQuarantined(ctx, e.ID, ResolvedApplied, "changed my mind", "rcpt", "eric"); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("second resolve: %v, want ErrInvalidTransition", err)
	}
	for _, q := range []string{"UPDATE effect_resolutions SET reason = 'x'", "DELETE FROM effect_resolutions"} {
		if _, err := st.Pool().Exec(ctx, q); err == nil {
			t.Errorf("audit is not append-only: %q accepted", q)
		}
	}
	// A resolved effect is terminal for the broker.
	if _, err := ab.Retry(ctx, e.ID); err == nil {
		t.Error("retry of a resolved effect was accepted")
	}
}

// Review N1 on aecf3fa: no new broker effect lands on a mission that is done.
func TestSubmitRefusedOnTerminalMission(t *testing.T) {
	ctx, st, b, fake := setup(t)
	ackRun(t, ctx, st)
	must := func(key string, cmd domain.Command) {
		t.Helper()
		if _, err := st.Execute(ctx, "fabd", domain.CommandID(key), cmd); err != nil {
			t.Fatalf("%s: %v", key, err)
		}
	}
	must("sel", domain.Command{Kind: domain.CmdSelectCandidate, Mission: "m_1", Head: "sha_t"})
	must("ev", domain.Command{Kind: domain.CmdRecordEvidence, Mission: "m_1", Attempt: "r_1", Fence: 1, Generation: 1, Head: "sha_t", Receipt: "rc"})
	must("fin", domain.Command{Kind: domain.CmdFinishAttempt, Mission: "m_1", Attempt: "r_1", Fence: 1, Outcome: "ready_for_review", Claims: []domain.Claim{{AC: "AC-1", Receipt: "rc"}}})
	must("ver", domain.Command{Kind: domain.CmdReconcileAttempt, Mission: "m_1", Attempt: "r_1", Fence: 1, Result: "VERIFIED"})
	must("acc", domain.Command{Kind: domain.CmdAccept, Mission: "m_1", Attempt: "r_1"})
	must("del", domain.Command{Kind: domain.CmdDeliver, Mission: "m_1", Attempt: "r_1", Generation: 1})
	must("live", domain.Command{Kind: domain.CmdRecordLiveTest, Mission: "m_1", Generation: 1, Head: "sha_t", Host: "h_t2", Target: "https://staging.example.test", Receipt: "rl"})
	must("done", domain.Command{Kind: domain.CmdCompleteMission, Mission: "m_1"})
	if _, err := b.Submit(ctx, fake, spec("kt1", "after done")); err == nil {
		t.Fatal("a broker effect was submitted on a terminal mission")
	}
}

// Review O1 on 717bb09: the audit binds every role, not only the runner.
func TestAppRoleCannotBypassEffectOutcomes(t *testing.T) {
	ctx, st, _, fake := setup(t)
	ackRun(t, ctx, st)
	runnerBroker(t, ctx, st) // creates the roles
	ab := appBroker(t, ctx, st)
	mk := func(key, state string) string {
		t.Helper()
		e, err := ab.Submit(ctx, fake, uspec(key))
		if err != nil {
			t.Fatal(err)
		}
		if state != "INTENT" {
			// Fixture setup as the owner, through the states the broker uses.
			for _, s := range map[string][]string{
				"QUARANTINED":     {"DISPATCHED", "UNKNOWN", "RECONCILING", "QUARANTINED"},
				"CONFIRMED":       {"DISPATCHED", "CONFIRMED"},
				"NOT_APPLIED":     {"DISPATCHED", "UNKNOWN", "RECONCILING", "NOT_APPLIED"},
				"FAILED_DEFINITE": {"DISPATCHED", "FAILED_DEFINITE"},
			}[state] {
				if _, err := st.Pool().Exec(ctx, "UPDATE effects SET receipt = CASE WHEN state = 'DISPATCHED' AND $2::text IN ('CONFIRMED', 'FAILED_DEFINITE') THEN jsonb_build_object('outcome', $2::text, 'dispatch', dispatches) ELSE receipt END, state = $2, dispatches = dispatches + CASE WHEN $2 = 'DISPATCHED' THEN 1 ELSE 0 END WHERE id = $1", e.ID, s); err != nil {
					t.Fatalf("fixture %s -> %s: %v", key, s, err)
				}
			}
		}
		return e.ID
	}
	q := mk("o1q", "QUARANTINED")
	c := mk("o1c", "CONFIRMED")
	n := mk("o1n", "NOT_APPLIED")
	f := mk("o1f", "FAILED_DEFINITE")
	for _, w := range []struct{ id, to string }{
		{q, "CONFIRMED"}, {q, "NOT_APPLIED"}, {q, "INTENT"}, {q, "UNKNOWN"},
		{c, "NOT_APPLIED"}, {c, "INTENT"}, {c, "QUARANTINED"},
		{n, "CONFIRMED"}, {n, "DISPATCHED"},
		{f, "CONFIRMED"},
	} {
		if _, err := ab.pool.Exec(ctx, "UPDATE effects SET state = $2 WHERE id = $1", w.id, w.to); err == nil {
			t.Errorf("app role moved %s to %s without the broker", w.id, w.to)
		}
	}
	for _, id := range []string{q, c} {
		if _, err := ab.pool.Exec(ctx, "DELETE FROM effects WHERE id = $1", id); err == nil {
			t.Errorf("app role deleted effect %s", id)
		}
	}
	// Retry's own moves stay open.
	if e, err := ab.Retry(ctx, f); err != nil || e.State != Intent {
		t.Errorf("retry of FAILED_DEFINITE: %+v %v", e, err)
	}
	if e, err := ab.Retry(ctx, n); err != nil || e.State != Intent {
		t.Errorf("retry of NOT_APPLIED: %+v %v", e, err)
	}
	// Review O4: the audit and the effects cannot be truncated away.
	for _, tbl := range []string{"effect_resolutions", "effects"} {
		if _, err := st.Pool().Exec(ctx, "TRUNCATE "+tbl+" CASCADE"); err == nil {
			t.Errorf("TRUNCATE %s accepted", tbl)
		}
	}
}

// blockingExec parks Precondition until release closes (review O3).
type blockingExec struct {
	*Fake
	entered chan struct{}
	release chan struct{}
}

func (b blockingExec) Precondition(ctx context.Context, e Effect) (bool, string, error) {
	close(b.entered)
	<-b.release
	return true, "", nil
}

// Review O3 on 717bb09: a dispatch parked in Precondition must not write to
// the remote after its attempt ended.
func TestDispatchAfterAttemptEndNoRemoteWrite(t *testing.T) {
	ctx, st, b, fake := setup(t)
	ackRun(t, ctx, st)
	e, err := b.Submit(ctx, fake, spec("o3", "late"))
	if err != nil {
		t.Fatal(err)
	}
	bx := blockingExec{Fake: fake, entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() { _, err := b.Dispatch(ctx, bx, e.ID); done <- err }()
	<-bx.entered
	if _, err := st.Execute(ctx, "fabd", "o3-sel", domain.Command{Kind: domain.CmdSelectCandidate, Mission: "m_1", Head: "sha_t"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Execute(ctx, "fabd", "o3-fin", domain.Command{Kind: domain.CmdFinishAttempt, Mission: "m_1", Attempt: "r_1", Fence: 1,
		Outcome: "ready_for_review", Claims: []domain.Claim{{AC: "AC-1", Receipt: "rc"}}}); err != nil {
		t.Fatal(err)
	}
	close(bx.release)
	if err := <-done; !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("dispatch after its attempt ended: %v, want the typed ErrInvalidTransition", err)
	}
	if fake.Count() != 0 {
		t.Fatalf("remote writes after the attempt ended: %d", fake.Count())
	}
	if got, _ := b.Load(ctx, e.ID); got.State != Intent {
		t.Fatalf("effect is %s, want INTENT", got.State)
	}
}

// Review O3: the move to DISPATCHED holds the attempt row, so ending the
// attempt waits for it, and done then sees the DISPATCHED row.
func TestDispatchedHoldsAttempt(t *testing.T) {
	ctx, st, b, fake := setup(t)
	ackRun(t, ctx, st)
	e, err := b.Submit(ctx, fake, spec("o3b", "held"))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := st.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck
	if _, err := tx.Exec(ctx, "UPDATE effects SET state = 'DISPATCHED', dispatches = dispatches + 1 WHERE id = $1", e.ID); err != nil {
		t.Fatal(err)
	}
	t2, err := st.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer t2.Rollback(context.Background()) //nolint:errcheck
	if _, err := t2.Exec(ctx, "SET LOCAL lock_timeout = '300ms'"); err != nil {
		t.Fatal(err)
	}
	if _, err := t2.Exec(ctx, "UPDATE attempts SET state = 'SUCCEEDED_UNVERIFIED' WHERE id = 'r_1'"); code(err) != "55P03" {
		t.Fatalf("ending the attempt under an uncommitted DISPATCHED: %v, want lock timeout 55P03", err)
	}
	// And a raw move to DISPATCHED on an ended attempt is refused.
	_ = t2.Rollback(ctx)
	_ = tx.Rollback(ctx)
	if _, err := st.Pool().Exec(ctx, "UPDATE attempts SET state = 'SUCCEEDED_UNVERIFIED' WHERE id = 'r_1'"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool().Exec(ctx, "UPDATE effects SET state = 'DISPATCHED', dispatches = dispatches + 1 WHERE id = $1", e.ID); err == nil {
		t.Fatal("DISPATCHED accepted on an ended attempt")
	}
}

// Review nit on 717bb09, main ruling: a never-dispatched effect left by an
// ended attempt blocks done; Abandon is its exit to NOT_APPLIED. App role,
// reason, evidence and actor, audited; dispatched effects go through resolve.
func TestAbandonStrandedIntent(t *testing.T) {
	ctx, st, b, fake := setup(t)
	ackRun(t, ctx, st)
	_, rb := runnerBroker(t, ctx, st)
	ab := appBroker(t, ctx, st)
	e, err := b.Submit(ctx, fake, spec("ab1", "stranded"))
	if err != nil {
		t.Fatal(err)
	}
	h, err := b.Submit(ctx, fake, uspec("ab4"))
	if err != nil {
		t.Fatal(err)
	}
	// One that was dispatched once, failed definitely, and was retried.
	d, err := b.Submit(ctx, fake, IntentSpec{Mission: "m_1", Attempt: "r_1", Fence: 1, Kind: "ticket_comment", Subject: "ticket:goodqa#1",
		IdemKey: "ab2", Request: json.RawMessage(`{"ticket": "goodqa#1", "body": "sent once"}`)})
	if err != nil {
		t.Fatal(err)
	}
	fake.Reject = true
	if got, err := b.Dispatch(ctx, fake, d.ID); err != nil || got.State != FailedDefinite {
		t.Fatalf("dispatch d: %+v %v", got, err)
	}
	fake.Reject = false
	if got, err := b.Retry(ctx, d.ID); err != nil || got.State != Intent || got.Dispatches != 1 {
		t.Fatalf("retry d: %+v %v", got, err)
	}
	if _, err := ab.Abandon(ctx, e.ID, "run ended", "wal:w_a", "eric"); !errors.Is(err, domain.ErrAttemptLive) {
		t.Errorf("abandon while the attempt is live: %v, want ErrAttemptLive", err)
	}
	if _, err := st.Pool().Exec(ctx, "UPDATE attempts SET state = 'SUCCEEDED_UNVERIFIED' WHERE id = 'r_1'"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, reason, evid, actor string }{
		{"no reason", "", "wal:w_a", "eric"}, {"no evidence", "run ended", "", "eric"}, {"no actor", "run ended", "wal:w_a", ""},
	} {
		if _, err := ab.Abandon(ctx, e.ID, c.reason, c.evid, c.actor); !errors.Is(err, domain.ErrInvalidTransition) {
			t.Errorf("abandon with %s: %v", c.name, err)
		}
	}
	if _, err := ab.Abandon(ctx, d.ID, "run ended", "wal:w_a", "eric"); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("abandon of a dispatched effect: %v, want ErrInvalidTransition", err)
	}
	if _, err := rb.Abandon(ctx, e.ID, "run ended", "wal:w_a", "runner"); err == nil {
		t.Error("the runner abandoned an effect")
	}
	// The database refuses the move without its audit row, and the
	// dispatched one even with a row (an earlier or forged one).
	if _, err := ab.pool.Exec(ctx, "UPDATE effects SET state = 'NOT_APPLIED' WHERE id = $1", e.ID); err == nil {
		t.Error("an unaudited abandon was accepted")
	}
	tx, err := ab.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO effect_resolutions (effect_id, from_state, to_state, reason, evidence, actor) VALUES ($1, 'INTENT', 'NOT_APPLIED', 'r', 'e', 'eric')`, d.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "UPDATE effects SET state = 'NOT_APPLIED' WHERE id = $1", d.ID); err == nil {
		t.Error("a dispatched effect was abandoned by a raw write with an audit row")
	}
	_ = tx.Rollback(ctx)
	got, err := ab.Abandon(ctx, e.ID, "run ended before dispatch", "wal:w_a", "eric")
	if err != nil || got.State != NotApplied || got.Dispatches != 0 {
		t.Fatalf("abandon: %+v %v", got, err)
	}
	var n int
	var from, to, actor, evid string
	if err := st.Pool().QueryRow(ctx, `SELECT count(*) OVER (), from_state, to_state, actor, evidence FROM effect_resolutions WHERE effect_id = $1`, e.ID).
		Scan(&n, &from, &to, &actor, &evid); err != nil || n != 1 || from != "INTENT" || to != "NOT_APPLIED" || actor != "eric" || evid != "wal:w_a" {
		t.Fatalf("abandon audit: n=%d %s->%s actor=%q evidence=%q err=%v", n, from, to, actor, evid, err)
	}
	// Review T2 on e35d78e: an abandoned effect is final; a retry would lose
	// its approval requirement and block done again.
	if _, err := ab.Retry(ctx, e.ID); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("retry of an abandoned effect: %v, want ErrInvalidTransition", err)
	}
	if _, err := ab.pool.Exec(ctx, "UPDATE effects SET state = 'INTENT' WHERE id = $1", e.ID); err == nil {
		t.Error("raw retry of an abandoned effect accepted")
	}
	// An audit row from an earlier transaction does not authorize a later
	// move: a row committed first, then a raw abandon, is refused.
	pre, err := ab.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pre.Exec(ctx, `INSERT INTO effect_resolutions (effect_id, from_state, to_state, reason, evidence, actor) VALUES ($1, 'INTENT', 'NOT_APPLIED', 'r', 'e', 'eric')`, h.ID); err != nil {
		t.Fatal(err)
	}
	if err := pre.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := ab.pool.Exec(ctx, "UPDATE effects SET state = 'NOT_APPLIED' WHERE id = $1", h.ID); err == nil {
		t.Error("an earlier audit row authorized a later abandon")
	}
}

// uspec is spec with its own subject, so fixtures can pass through the
// live-subject states side by side.
func uspec(key string) IntentSpec {
	s := spec(key, key)
	s.Subject = "ticket:goodqa#" + key
	return s
}

// Review Q1/Q2 on 3f38cc3 (main ruling): after insert every column is
// immutable except an allowlist (state, dispatches by the dispatch path,
// receipt while unsettled, the approval on approve, last_error, updated_at).
func TestEffectColumnsImmutable(t *testing.T) {
	ctx, st, b, fake := setup(t)
	ackRun(t, ctx, st)
	runnerBroker(t, ctx, st)
	ab := appBroker(t, ctx, st)
	if err := st.CreateMission(ctx, "m_2", domain.ProviderClaudeCode, "GoodQA goodqa#14791", "fabric", "main"); err != nil {
		t.Fatal(err)
	}
	fake.LoseAfter = true
	unk, _ := b.Submit(ctx, fake, uspec("q1u"))
	if unk, _ = b.Dispatch(ctx, fake, unk.ID); unk.State != Unknown {
		t.Fatalf("setup unknown: %s", unk.State)
	}
	fake.LoseAfter = false
	ok, _ := b.Submit(ctx, fake, uspec("q1c"))
	if ok, _ = b.Dispatch(ctx, fake, ok.ID); ok.State != Confirmed {
		t.Fatalf("setup confirmed: %s", ok.State)
	}
	fake.Reject = true
	re, _ := b.Submit(ctx, fake, uspec("q1r"))
	if re, _ = b.Dispatch(ctx, fake, re.ID); re.State != FailedDefinite {
		t.Fatalf("setup failed: %s", re.State)
	}
	fake.Reject = false
	if re, _ = b.Retry(ctx, re.ID); re.State != Intent || re.Dispatches != 1 {
		t.Fatalf("setup retried: %+v", re)
	}
	for _, c := range []struct{ name, q, id string }{
		{"Q1b move to another mission", "UPDATE effects SET mission_id = 'm_2' WHERE id = $1", unk.ID},
		{"request rewritten", `UPDATE effects SET request = '{"ticket":"goodqa#0","body":"swapped"}' WHERE id = $1`, unk.ID},
		{"request hash rewritten", "UPDATE effects SET request_sha = 'x' WHERE id = $1", unk.ID},
		{"subject", "UPDATE effects SET subject = 'elsewhere' WHERE id = $1", unk.ID},
		{"kind", "UPDATE effects SET kind = 'other' WHERE id = $1", unk.ID},
		{"key", "UPDATE effects SET idem_key = 'k_new' WHERE id = $1", unk.ID},
		{"attempt", "UPDATE effects SET attempt_id = 'r_9' WHERE id = $1", unk.ID},
		{"fence", "UPDATE effects SET fence = 9 WHERE id = $1", unk.ID},
		{"created_at", "UPDATE effects SET created_at = now() - interval '1 day' WHERE id = $1", unk.ID},
		{"id", "UPDATE effects SET id = 'e_new' WHERE id = $1", unk.ID},
		{"settled receipt", `UPDATE effects SET receipt = '{"comment_id":"forged"}' WHERE id = $1`, ok.ID},
		{"Q2 dispatches reset", "UPDATE effects SET dispatches = 0 WHERE id = $1", re.ID},
		{"dispatch without the count", "UPDATE effects SET state = 'DISPATCHED' WHERE id = $1", re.ID},
		{"dispatch count jump", "UPDATE effects SET state = 'DISPATCHED', dispatches = dispatches + 2 WHERE id = $1", re.ID},
		{"approval outside approve", "UPDATE effects SET approval_sha = request_sha, approved_by = 'eric' WHERE id = $1", re.ID},
	} {
		if _, err := ab.pool.Exec(ctx, c.q, c.id); err == nil {
			t.Errorf("app role: %s accepted", c.name)
		}
	}
	// Allowed: a diagnostic note while the outcome is open.
	if _, err := ab.pool.Exec(ctx, "UPDATE effects SET last_error = 'note', updated_at = now() WHERE id = $1", unk.ID); err != nil {
		t.Errorf("last_error note on an open effect refused: %v", err)
	}
}

// Review Q4 on 3f38cc3: an effect is born INTENT or APPROVAL_REQUIRED,
// never sent, with no receipt or approval.
func TestEffectInsertOnlyInitial(t *testing.T) {
	ctx, st, _, _ := setup(t)
	ackRun(t, ctx, st)
	runnerBroker(t, ctx, st)
	ab := appBroker(t, ctx, st)
	ins := `INSERT INTO effects (id, mission_id, attempt_id, fence, kind, subject, idem_key, request, request_sha, state, dispatches, receipt, approval_sha, approved_by)
		VALUES ($1, 'm_1', 'r_1', 1, 'ticket_comment', $1, $1, '{}', 'sha', $2, $3, $4, $5, $6)`
	for _, c := range []struct {
		name, state string
		dispatches  int
		receipt     any
		sha, by     any
	}{
		{"born confirmed", "CONFIRMED", 1, `{"comment_id":"c"}`, nil, nil},
		{"born not applied", "NOT_APPLIED", 0, nil, nil, nil},
		{"born dispatched", "DISPATCHED", 1, nil, nil, nil},
		{"born approved", "APPROVED", 0, nil, "sha", "eric"},
		{"born failed", "FAILED_DEFINITE", 0, nil, nil, nil},
		{"intent already sent", "INTENT", 1, nil, nil, nil},
		{"intent with a receipt", "INTENT", 0, `{"comment_id":"c"}`, nil, nil},
		{"intent with an approval", "APPROVAL_REQUIRED", 0, nil, "sha", "eric"},
	} {
		if _, err := ab.pool.Exec(ctx, ins, "e_"+c.state+fmt.Sprint(c.dispatches), c.state, c.dispatches, c.receipt, c.sha, c.by); err == nil {
			t.Errorf("app role inserted an effect %s", c.name)
		}
	}
	if _, err := ab.pool.Exec(ctx, ins, "e_fine", "INTENT", 0, nil, nil, nil); err != nil {
		t.Errorf("a plain intent insert was refused: %v", err)
	}
}

// Review Q3 on 3f38cc3: the fence is the effect's own mission's attempt, and
// that mission is not terminal.
func TestFenceBelongsToMission(t *testing.T) {
	ctx, st, b, fake := setup(t)
	ackRun(t, ctx, st)
	runnerBroker(t, ctx, st)
	ab := appBroker(t, ctx, st)
	if err := st.CreateMission(ctx, "m_2", domain.ProviderClaudeCode, "GoodQA goodqa#14791", "fabric", "main"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		key string
		cmd domain.Command
	}{
		{"a2", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_2"}},
		{"s2", domain.Command{Kind: domain.CmdStartAttempt, Path: domain.ProviderClaudeCode, Mission: "m_2", Attempt: "r_2", Host: "h_t2", Role: "writer"}},
		{"k2", domain.Command{Kind: domain.CmdAckStart, Mission: "m_2", Attempt: "r_2", Fence: 1}},
	} {
		if _, err := st.Execute(ctx, "fabd", domain.CommandID(c.key), c.cmd); err != nil {
			t.Fatalf("%s: %v", c.key, err)
		}
	}
	cross := uspec("q3")
	cross.Attempt, cross.Fence = "r_2", 1 // m_2's live attempt, on m_1
	if _, err := b.Submit(ctx, fake, cross); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("submit with another mission's attempt: %v, want ErrInvalidTransition", err)
	}
	if _, err := ab.pool.Exec(ctx, `INSERT INTO effects (id, mission_id, attempt_id, fence, kind, subject, idem_key, request, request_sha, state)
		VALUES ('e_q3', 'm_1', 'r_2', 1, 'ticket_comment', 's_q3', 'k_q3', '{}', 'sha', 'INTENT')`); err == nil {
		t.Error("raw insert with another mission's attempt accepted")
	}
	if fake.Count() != 0 {
		t.Fatalf("remote writes: %d", fake.Count())
	}
}

// Review S1-S6 on 4ae0565: state moves are an allow-list too, mirroring the
// broker, so a pair it does not name is refused for every role.
func TestEffectTransitionsAllowListed(t *testing.T) {
	ctx, st, b, fake := setup(t)
	ackRun(t, ctx, st)
	runnerBroker(t, ctx, st)
	ab := appBroker(t, ctx, st)
	fake.LoseAfter = true
	unk, _ := b.Submit(ctx, fake, uspec("s1u"))
	if unk, _ = b.Dispatch(ctx, fake, unk.ID); unk.State != Unknown {
		t.Fatalf("setup unknown: %s", unk.State)
	}
	fake.LoseAfter = false
	in, _ := b.Submit(ctx, fake, uspec("s3i"))
	ap := uspec("s2a")
	ap.NeedsApproval = true
	apr, _ := b.Submit(ctx, fake, ap)
	ok, _ := b.Submit(ctx, fake, uspec("s6c"))
	if ok, _ = b.Dispatch(ctx, fake, ok.ID); ok.State != Confirmed {
		t.Fatalf("setup confirmed: %s", ok.State)
	}
	// An approved effect that failed definitely must retry to APPROVED.
	apf := uspec("s2f")
	apf.NeedsApproval = true
	af, _ := b.Submit(ctx, fake, apf)
	if err := b.Approve(ctx, af.ID, af.RequestSHA, "eric"); err != nil {
		t.Fatal(err)
	}
	fake.Reject = true
	if af, _ = b.Dispatch(ctx, fake, af.ID); af.State != FailedDefinite {
		t.Fatalf("setup approved-failed: %s", af.State)
	}
	fake.Reject = false
	for _, c := range []struct{ name, to, id string }{
		{"S1 unknown re-armed", "INTENT", unk.ID},
		{"unknown settled without readback", "CONFIRMED", unk.ID},
		{"unknown not-applied without readback", "NOT_APPLIED", unk.ID},
		{"S2 approval skipped", "INTENT", apr.ID},
		{"S3 never-sent confirmed", "CONFIRMED", in.ID},
		{"S5 never-sent quarantined", "QUARANTINED", in.ID},
		{"approved retry dropped its approval", "INTENT", af.ID},
	} {
		if _, err := ab.pool.Exec(ctx, "UPDATE effects SET state = $2 WHERE id = $1", c.id, c.to); err == nil {
			t.Errorf("app role: %s (-> %s) accepted", c.name, c.to)
		}
	}
	if _, err := ab.pool.Exec(ctx, "UPDATE effects SET last_error = 'rewritten' WHERE id = $1", ok.ID); err == nil {
		t.Error("S6: last_error rewritten on a settled effect")
	}
	// The broker's own moves still pass: readback, retry to APPROVED.
	if got, err := ab.Reconcile(ctx, fake, unk.ID); err != nil || got.State != Confirmed {
		t.Errorf("reconcile of the unknown effect: %+v %v", got, err)
	}
	if got, err := ab.Retry(ctx, af.ID); err != nil || got.State != Approved {
		t.Errorf("retry of an approved failure: %+v %v", got, err)
	}
	// S4: an in-flight effect cannot be re-armed either.
	tx, err := ab.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck
	if _, err := tx.Exec(ctx, "UPDATE effects SET state = 'DISPATCHED', dispatches = dispatches + 1 WHERE id = $1", in.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "UPDATE effects SET state = 'INTENT' WHERE id = $1", in.ID); err == nil {
		t.Error("S4: DISPATCHED re-armed to INTENT")
	}
}

// Review S2: Dispatch itself refuses an effect that carries an approval but
// is not APPROVED (the second layer, exercised with the guard off).
func TestDispatchRefusesUnapprovedIntentWithApproval(t *testing.T) {
	ctx, st, b, fake := setup(t)
	e, _ := b.Submit(ctx, fake, uspec("s2g"))
	tx, err := st.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"ALTER TABLE effects DISABLE TRIGGER effects_zz_outcome_guard",
		"UPDATE effects SET approval_sha = request_sha, approved_by = 'x' WHERE id = '" + e.ID + "'",
		"ALTER TABLE effects ENABLE TRIGGER effects_zz_outcome_guard"} {
		if _, err := tx.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Dispatch(ctx, fake, e.ID); !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("dispatch of an INTENT carrying an approval: %v, want ErrApprovalRequired", err)
	}
	if fake.Count() != 0 {
		t.Fatalf("remote writes %d", fake.Count())
	}
}

// Review T1 on e35d78e: a DISPATCHED effect left by a crash (the remote may
// have applied it) has a supported exit once its attempt is no longer live:
// Unresolved lists it and Reconcile reads it back. A raw settle needs a
// matching new receipt, so it cannot be relabelled FAILED_DEFINITE and sent
// again.
func TestStaleDispatchedRecovers(t *testing.T) {
	ctx, st, b, fake := setup(t)
	b.SetTransportTimeout(100 * time.Millisecond)
	ackRun(t, ctx, st)
	runnerBroker(t, ctx, st)
	ab := appBroker(t, ctx, st)
	crashOf := func(e Effect) Effect {
		t.Helper()
		b.afterTransport = func(Effect, Receipt) { panic("crash after transport") }
		func() {
			defer func() { recover() }() //nolint:errcheck
			b.Dispatch(ctx, fake, e.ID)  //nolint:errcheck
		}()
		b.afterTransport = nil
		e, _ = b.Load(ctx, e.ID)
		if e.State != Dispatched {
			t.Fatalf("setup %s: %s", e.IdemKey, e.State)
		}
		return e
	}
	crash := func(key string) Effect {
		t.Helper()
		e, err := b.Submit(ctx, fake, uspec(key))
		if err != nil {
			t.Fatal(err)
		}
		return crashOf(e)
	}
	e := crash("t1a")
	f := crash("t1b")
	// One that failed definitely and was retried still carries that
	// receipt while DISPATCHED; reusing it is not a new outcome.
	fake.Reject = true
	g, err := b.Submit(ctx, fake, uspec("t1c"))
	if err != nil {
		t.Fatal(err)
	}
	if g, err = b.Dispatch(ctx, fake, g.ID); err != nil || g.State != FailedDefinite {
		t.Fatalf("setup t1c: %s %v", g.State, err)
	}
	fake.Reject = false
	if g, err = b.Retry(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	g = crashOf(g)
	if fake.Count() != 3 {
		t.Fatalf("remote writes %d, want 3", fake.Count())
	}
	// Past the dispatch's stamped deadline and the reconciler's (the
	// deadline itself is covered by TestDispatcherSessionLossKeepsOutcome and
	// TestStaleFollowsStampedDeadline), so here only attempt liveness protects.
	ab.SetTransportTimeout(100 * time.Millisecond)
	time.Sleep(250 * time.Millisecond)
	// While the attempt is live the dispatcher may still settle it.
	if _, err := ab.Reconcile(ctx, fake, e.ID); !errors.Is(err, ErrNotReconcilable) {
		t.Errorf("reconcile of a live attempt's DISPATCHED effect: %v, want ErrNotReconcilable", err)
	}
	if ids, _ := ab.Unresolved(ctx, 0, 10); len(ids) != 0 {
		t.Errorf("unresolved lists live dispatches: %v", ids)
	}
	if _, err := st.Pool().Exec(ctx, "UPDATE attempts SET state = 'UNKNOWN' WHERE id = 'r_1'"); err != nil {
		t.Fatal(err)
	}
	// A raw settle without a matching new receipt is refused (RT-T1).
	for _, q := range []string{
		"UPDATE effects SET state = 'FAILED_DEFINITE' WHERE id = $1",
		"UPDATE effects SET state = 'CONFIRMED' WHERE id = $1",
		`UPDATE effects SET state = 'FAILED_DEFINITE', receipt = '{"outcome":"CONFIRMED","dispatch":1}' WHERE id = $1`,
	} {
		if _, err := ab.pool.Exec(ctx, q, f.ID); err == nil {
			t.Errorf("raw settle accepted: %s", q)
		}
	}
	if _, err := ab.pool.Exec(ctx, "UPDATE effects SET state = 'FAILED_DEFINITE' WHERE id = $1", g.ID); err == nil {
		t.Error("raw settle reusing the previous FAILED_DEFINITE receipt accepted")
	}
	ids, err := ab.Unresolved(ctx, 0, 10)
	if err != nil || len(ids) != 3 {
		t.Fatalf("unresolved after the attempt ended: %v %v, want all three", ids, err)
	}
	for _, id := range []string{e.ID, f.ID, g.ID} {
		got, err := ab.Reconcile(ctx, fake, id)
		if err != nil || got.State != Confirmed {
			t.Errorf("reconcile of stale %s: %+v %v", id, got, err)
		}
	}
	if fake.Count() != 3 {
		t.Fatalf("recovery sent again: remote writes %d", fake.Count())
	}
}

// parkedSend parks the transport until release closes, after the effect
// is committed DISPATCHED (review U1).
type parkedSend struct {
	*Fake
	entered chan struct{}
	release chan struct{}
}

func (p parkedSend) Dispatch(ctx context.Context, sub Submission) (Receipt, error) {
	close(p.entered)
	<-p.release
	return p.Fake.Dispatch(ctx, sub)
}

// Review U1 on 75e4e4d: a transport still in flight when its attempt ends
// is not stale. Unresolved skips it and Reconcile refuses it while the
// dispatcher holds the effect's lock; the real outcome is then recorded.
// A settle that finds the row moved on is reported, not dropped.
func TestInFlightDispatchNotReconciled(t *testing.T) {
	ctx, st, b, fake := setup(t)
	b.SetTransportTimeout(50 * time.Millisecond)
	ackRun(t, ctx, st)
	runnerBroker(t, ctx, st)
	ab := appBroker(t, ctx, st)
	park := func(key string) (Effect, parkedSend, chan error) {
		t.Helper()
		e, err := b.Submit(ctx, fake, uspec(key))
		if err != nil {
			t.Fatal(err)
		}
		ps := parkedSend{Fake: fake, entered: make(chan struct{}), release: make(chan struct{})}
		done := make(chan error, 1)
		go func() { _, err := b.Dispatch(ctx, ps, e.ID); done <- err }()
		<-ps.entered
		return e, ps, done
	}
	// A concurrent raw move under the dispatcher: its settle is reported.
	f, ps2, done2 := park("u1b")
	if _, err := st.Pool().Exec(ctx, "UPDATE effects SET state = 'UNKNOWN' WHERE id = $1", f.ID); err != nil {
		t.Fatal(err)
	}
	close(ps2.release)
	if err := <-done2; !errors.Is(err, ErrSettleLost) {
		t.Errorf("settle on a moved row: %v, want ErrSettleLost", err)
	}
	if b.SettleLost() != 1 {
		t.Errorf("settle lost count %d, want 1", b.SettleLost())
	}
	e, ps, done := park("u1a")
	if _, err := st.Pool().Exec(ctx, "UPDATE attempts SET state = 'UNKNOWN' WHERE id = 'r_1'"); err != nil {
		t.Fatal(err)
	}
	// Past the stamped and the reconciler's deadline, so only the
	// dispatcher's lock protects it.
	ab.SetTransportTimeout(50 * time.Millisecond)
	time.Sleep(150 * time.Millisecond)
	ids, err := ab.Unresolved(ctx, 0, 10)
	if err != nil || len(ids) != 1 || ids[0] != f.ID {
		t.Errorf("unresolved: %v %v, want only the UNKNOWN %s, not the in-flight %s", ids, err, f.ID, e.ID)
	}
	if _, err := ab.Reconcile(ctx, fake, e.ID); !errors.Is(err, ErrNotReconcilable) {
		t.Errorf("reconcile under an in-flight dispatch: %v, want ErrNotReconcilable", err)
	}
	close(ps.release)
	if err := <-done; err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if got, _ := ab.Load(ctx, e.ID); got.State != Confirmed || fake.Count() != 2 {
		t.Fatalf("after the transport landed: %s remote=%d, want CONFIRMED 2", got.State, fake.Count())
	}
}

// slowSend parks every transport until release closes or its context ends.
type slowSend struct {
	*Fake
	entered chan string
	release chan struct{}
}

func (s slowSend) Dispatch(ctx context.Context, sub Submission) (Receipt, error) {
	s.entered <- sub.Effect.ID
	select {
	case <-s.release:
		return s.Fake.Dispatch(ctx, sub)
	case <-ctx.Done():
		return Receipt{}, ctx.Err()
	}
}

// Main on 25fb014: a dispatch holds a connection for its transport, so
// in-flight dispatches are capped below the pool size; N+1 slow sends leave
// the reconcile and command paths free.
func TestSlowDispatchesDoNotStarvePool(t *testing.T) {
	ctx, st, b, fake := setup(t)
	ackRun(t, ctx, st)
	n := b.DispatchLimit()
	if n < 1 || n >= int(st.Pool().Config().MaxConns) {
		t.Fatalf("dispatch limit %d, want 1..%d", n, st.Pool().Config().MaxConns-1)
	}
	ss := slowSend{Fake: fake, entered: make(chan string, n+1), release: make(chan struct{})}
	done := make(chan error, n+1)
	var ids []string
	for i := 0; i <= n; i++ {
		e, err := b.Submit(ctx, fake, uspec(fmt.Sprintf("slow%d", i)))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, e.ID)
		go func(id string) { _, err := b.Dispatch(ctx, ss, id); done <- err }(e.ID)
	}
	for i := 0; i < n; i++ {
		<-ss.entered
	}
	qctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var one int
	if err := st.Pool().QueryRow(qctx, "SELECT 1").Scan(&one); err != nil {
		t.Fatalf("command path blocked by slow dispatches: %v", err)
	}
	if _, err := b.Unresolved(qctx, 0, 10); err != nil {
		t.Fatalf("unresolved blocked: %v", err)
	}
	if _, err := b.Reconcile(qctx, fake, ids[0]); !errors.Is(err, ErrNotReconcilable) && !errors.Is(err, ErrNotDispatchable) {
		t.Fatalf("reconcile blocked or wrong: %v", err)
	}
	select {
	case id := <-ss.entered:
		t.Fatalf("dispatch %s entered transport beyond the limit %d", id, n)
	default:
	}
	close(ss.release)
	for i := 0; i <= n; i++ {
		if err := <-done; err != nil {
			t.Errorf("dispatch: %v", err)
		}
	}
	if fake.Count() != n+1 {
		t.Fatalf("remote writes %d, want %d", fake.Count(), n+1)
	}
}

// Main on 25fb014: the transport runs under a deadline below the lease; a
// hung remote settles UNKNOWN for readback instead of holding the lock.
func TestTransportTimeoutSettlesUnknown(t *testing.T) {
	ctx, st, b, fake := setup(t)
	ackRun(t, ctx, st)
	b.SetTransportTimeout(200 * time.Millisecond)
	ss := slowSend{Fake: fake, entered: make(chan string, 1), release: make(chan struct{})}
	e, err := b.Submit(ctx, fake, uspec("hang"))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	got, err := b.Dispatch(ctx, ss, e.ID)
	if err != nil || got.State != Unknown || time.Since(start) > 3*time.Second {
		t.Fatalf("hung transport: %s %v after %s, want UNKNOWN promptly", got.State, err, time.Since(start))
	}
}

// Review V1 on 25fb014: the dispatcher's session dies mid-transport (the
// lock dies with it). The effect is not stale before the transport deadline,
// and the outcome is recorded on a fresh connection.
func TestDispatcherSessionLossKeepsOutcome(t *testing.T) {
	ctx, st, b, fake := setup(t)
	ackRun(t, ctx, st)
	runnerBroker(t, ctx, st)
	ab := appBroker(t, ctx, st)
	e, err := b.Submit(ctx, fake, uspec("v1"))
	if err != nil {
		t.Fatal(err)
	}
	ps := parkedSend{Fake: fake, entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() { _, err := b.Dispatch(ctx, ps, e.ID); done <- err }()
	<-ps.entered
	var killed bool
	if err := st.Pool().QueryRow(ctx, `SELECT pg_terminate_backend(pid) FROM pg_locks WHERE locktype = 'advisory'
		AND classid = 4011 AND objid = hashtext($1)::oid AND objsubid = 2`, e.ID).Scan(&killed); err != nil || !killed {
		t.Fatalf("terminate the dispatcher session: %v %v", killed, err)
	}
	if _, err := st.Pool().Exec(ctx, "UPDATE attempts SET state = 'UNKNOWN' WHERE id = 'r_1'"); err != nil {
		t.Fatal(err)
	}
	if ids, _ := ab.Unresolved(ctx, 0, 10); len(ids) != 0 {
		t.Errorf("unresolved lists a dispatch inside its transport deadline: %v", ids)
	}
	if _, err := ab.Reconcile(ctx, fake, e.ID); !errors.Is(err, ErrNotReconcilable) {
		t.Errorf("reconcile inside the transport deadline: %v, want ErrNotReconcilable", err)
	}
	close(ps.release)
	if err := <-done; err != nil {
		t.Fatalf("dispatch after its session died: %v", err)
	}
	if got, _ := ab.Load(ctx, e.ID); got.State != Confirmed || fake.Count() != 1 {
		t.Fatalf("after session loss: %s remote=%d, want CONFIRMED 1", got.State, fake.Count())
	}
}
