package effects

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"testing"

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
	if _, err := st.Pool().Exec(ctx, "UPDATE effects SET state = 'QUARANTINED' WHERE id = $1", e.ID); err != nil {
		t.Fatal(err)
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
