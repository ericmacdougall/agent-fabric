package effects

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"fabric/internal/domain"
	"fabric/internal/store"
)

// runnerBroker applies the roles and opens the broker as the runner role,
// as a worker's `fab effect` does.
func runnerBroker(t *testing.T, ctx context.Context, st *store.Store) (*store.Store, *Broker) {
	t.Helper()
	r := store.Roles{Prefix: fmt.Sprintf("fabric_e%d", os.Getpid())}
	if err := st.ApplyRoles(ctx, r, "", ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, n := range []string{r.App(), r.Runner(), r.Scheduler()} {
			st.Pool().Exec(context.Background(), "DROP OWNED BY "+n) //nolint:errcheck
			st.Pool().Exec(context.Background(), "DROP ROLE "+n)     //nolint:errcheck
		}
	})
	u, err := url.Parse(testDSN)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("role", r.Runner())
	u.RawQuery = q.Encode()
	rs, err := store.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rs.Close)
	return rs, New(rs)
}

func code(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

func ackRun(t *testing.T, ctx context.Context, st *store.Store) {
	t.Helper()
	if _, err := st.Execute(ctx, "fabd", "ack", domain.Command{Kind: domain.CmdAckStart, Mission: "m_1", Attempt: "r_1", Fence: 1}); err != nil {
		t.Fatal(err)
	}
}

// Red-team J1-J3 on eef7ca7: as the runner, only the broker's runner-side
// transitions go through. The controls are the real broker flows run as
// the runner; reconcile is the app role's.
func TestRunnerRoleBrokerFlows(t *testing.T) {
	ctx, st, b, fake := setup(t)
	ackRun(t, ctx, st)
	_, rb := runnerBroker(t, ctx, st)

	// Control: submit and dispatch.
	e, err := rb.Submit(ctx, fake, spec("kr1", "hello"))
	if err != nil {
		t.Fatalf("control: runner submit: %v", err)
	}
	if e, err = rb.Dispatch(ctx, fake, e.ID); err != nil || e.State != Confirmed || e.Dispatches != 1 {
		t.Fatalf("control: runner dispatch: %+v %v", e, err)
	}
	// Control: an approved effect dispatches; the approval is not the runner's.
	s := spec("kr2", "publish")
	s.NeedsApproval = true
	ap, err := rb.Submit(ctx, fake, s)
	if err != nil || ap.State != ApprovalRequired {
		t.Fatalf("control: runner submit needing approval: %+v %v", ap, err)
	}
	if err := rb.Approve(ctx, ap.ID, ap.RequestSHA, "runner"); code(err) != "42501" {
		t.Errorf("runner approved its own effect: %v, want 42501", err)
	}
	// Control: lost request, readback by the app role, retry by the runner.
	fake.LoseBefor = true
	lost, _ := rb.Submit(ctx, fake, spec("kr3", "once"))
	if lost, err = rb.Dispatch(ctx, fake, lost.ID); err != nil || lost.State != Unknown {
		t.Fatalf("control: lost dispatch: %+v %v", lost, err)
	}
	fake.LoseBefor = false
	if _, err := rb.Reconcile(ctx, fake, lost.ID); code(err) != "42501" {
		t.Errorf("runner reconciled: %v, want 42501", err)
	}
	if lost, err = b.Reconcile(ctx, fake, lost.ID); err != nil || lost.State != NotApplied {
		t.Fatalf("control: app reconcile: %+v %v", lost, err)
	}
	if lost, err = rb.Retry(ctx, lost.ID); err != nil || lost.State != Intent {
		t.Fatalf("control: runner retry: %+v %v", lost, err)
	}
	if lost, err = rb.Dispatch(ctx, fake, lost.ID); err != nil || lost.State != Confirmed || lost.Dispatches != 2 {
		t.Fatalf("control: runner redispatch: %+v %v", lost, err)
	}

	// An UNKNOWN effect for the J2 case; an INTENT one for J3.
	fake.LoseAfter = true
	unk, _ := rb.Submit(ctx, fake, spec("kr4", "applied"))
	if unk, err = rb.Dispatch(ctx, fake, unk.ID); err != nil || unk.State != Unknown {
		t.Fatalf("setup: %+v %v", unk, err)
	}
	fake.LoseAfter = false
	in, err := rb.Submit(ctx, fake, spec("kr5", "unsent"))
	if err != nil {
		t.Fatal(err)
	}
	rs := rb.pool
	for _, c := range []struct{ name, q, id string }{
		{"J1 approval bypass", "UPDATE effects SET state = 'INTENT' WHERE id = $1", ap.ID},
		{"approval-required to failed", "UPDATE effects SET state = 'FAILED_DEFINITE' WHERE id = $1", ap.ID},
		{"J2 applied to not-applied", "UPDATE effects SET state = 'NOT_APPLIED' WHERE id = $1", unk.ID},
		{"unknown to reconciling", "UPDATE effects SET state = 'RECONCILING' WHERE id = $1", unk.ID},
		{"unknown to quarantined", "UPDATE effects SET state = 'QUARANTINED' WHERE id = $1", unk.ID},
		{"J3 intent to confirmed", `UPDATE effects SET state = 'CONFIRMED', receipt = '{"comment_id":"forged"}' WHERE id = $1`, in.ID},
		{"dispatch count forged", "UPDATE effects SET state = 'DISPATCHED', dispatches = dispatches + 7 WHERE id = $1", in.ID},
		{"dispatch count rewound", "UPDATE effects SET dispatches = 0 WHERE id = $1", e.ID},
		{"confirmed rewritten", "UPDATE effects SET state = 'FAILED_DEFINITE' WHERE id = $1", e.ID},
	} {
		if _, err := rs.Exec(ctx, c.q, c.id); code(err) != "42501" {
			t.Errorf("runner: %s gave %v, want 42501", c.name, err)
		}
	}
	// Red-team K2 on 4d8e1de: a crash-left DISPATCHED effect of an ended run
	// is the app role's readback, not a runner settle.
	// The fixture is born an intent of r_1, dispatched, then r_1 ends.
	for _, q := range []string{
		`INSERT INTO effects (id, mission_id, attempt_id, fence, kind, subject, idem_key, request, request_sha, state)
			VALUES ('e_crash', 'm_1', 'r_1', 1, 'ticket_comment', 's_crash', 'k_crash', '{}', 'sha', 'INTENT')`,
		"UPDATE effects SET state = 'DISPATCHED', dispatches = 1 WHERE id = 'e_crash'",
		"UPDATE attempts SET state = 'SUCCEEDED_UNVERIFIED' WHERE id = 'r_1'",
	} {
		if _, err := st.Pool().Exec(ctx, q); err != nil {
			t.Fatalf("fixture %q: %v", q, err)
		}
	}
	for _, to := range []string{"CONFIRMED", "FAILED_DEFINITE", "UNKNOWN"} {
		if _, err := rs.Exec(ctx, `UPDATE effects SET state = $2, receipt = '{"comment_id":"forged"}' WHERE id = $1`, "e_crash", to); code(err) != "42501" {
			t.Errorf("runner settled an ended run's dispatch as %s: %v, want 42501", to, err)
		}
	}
	// Review nit on 2afeffe: the refused settle names the remedy.
	rc, err := rb.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Release()
	if err := rb.settle(ctx, rc, "e_crash", 1, Receipt{Outcome: OutcomeConfirmed}); !errors.Is(err, ErrSettleRunEnded) {
		t.Errorf("settle after the run ended: %v, want ErrSettleRunEnded", err)
	}
	if n := rb.SettleRefused(); n != 1 {
		t.Errorf("settle refusals counted %d, want 1", n)
	}
	// An effect for an attempt that is not running.
	other := spec("kr6", "elsewhere")
	other.Attempt, other.Fence = "r_gone", 1
	if _, err := rb.Submit(ctx, fake, other); code(err) != "42501" && !errors.Is(err, domain.ErrNotFound) && !errors.Is(err, domain.ErrStaleFence) {
		t.Errorf("runner submitted an effect for an attempt that is not running: %v", err)
	}
	if _, err := rs.Exec(ctx, `INSERT INTO effects (id, mission_id, attempt_id, fence, kind, subject, idem_key, request, request_sha, state)
		VALUES ('e_x', 'm_1', 'r_gone', 1, 'ticket_comment', 's_x', 'k_x', '{}', 'sha', 'INTENT')`); code(err) != "42501" {
		t.Errorf("runner: raw effect insert for a non-running attempt gave %v, want 42501", err)
	}
}
