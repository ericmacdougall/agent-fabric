package effects

import (
	"errors"
	"testing"

	"fabric/internal/domain"
)

// goodqa#15166 review F1(a): comment, FAILED_DEFINITE, the attempt ends, then
// the operator re-runs the same comment (the CLI's replay calls Retry). The
// old Retry re-armed the effect to INTENT; its dispatch could never pass the
// live fence, and abandon refuses an intent with dispatches > 0, so the
// effect was stranded and the mission un-admittable for good. Retry now
// refuses unless the attempt is live at the effect's fence, and the effect
// stays FAILED_DEFINITE (settled). While the attempt is live, retry works.
func TestRetryNeedsLiveAttempt(t *testing.T) {
	ctx, st, b, fake := setup(t)
	if _, err := st.Execute(ctx, "fabd", "ack-r1", domain.Command{Kind: domain.CmdAckStart, Mission: "m_1", Attempt: "r_1", Fence: 1}); err != nil {
		t.Fatal(err)
	}
	fake.Reject = true

	// While r_1 is live, a definite failure can be retried and dispatched.
	live, err := b.Submit(ctx, fake, spec("k-live", "first"))
	if err != nil {
		t.Fatal(err)
	}
	if live, err = b.Dispatch(ctx, fake, live.ID); err != nil || live.State != FailedDefinite {
		t.Fatalf("setup dispatch: %s %v", live.State, err)
	}
	if live, err = b.Retry(ctx, live.ID); err != nil || live.State != Intent {
		t.Fatalf("retry with a live attempt: %s %v; want INTENT", live.State, err)
	}

	// The comment that will be retried after the attempt ends.
	e, err := b.Submit(ctx, fake, spec("k-dead", "second"))
	if err != nil {
		t.Fatal(err)
	}
	if e, err = b.Dispatch(ctx, fake, e.ID); err != nil || e.State != FailedDefinite || e.Dispatches != 1 {
		t.Fatalf("setup dispatch: %s %v dispatches=%d", e.State, err, e.Dispatches)
	}
	// Settle the re-armed one again (the remote still refuses) so nothing is
	// left in flight when the attempt ends.
	if live, err = b.Dispatch(ctx, fake, live.ID); err != nil || live.State != FailedDefinite {
		t.Fatalf("re-dispatch: %s %v", live.State, err)
	}
	if _, err := st.Execute(ctx, "fabd", "fin-r1", domain.Command{Kind: domain.CmdFinishAttempt, Mission: "m_1", Attempt: "r_1", Fence: 1,
		Outcome: "abandoned", Reason: "test"}); err != nil {
		t.Fatal(err)
	}

	got, err := b.Retry(ctx, e.ID)
	if err == nil || !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("retry after the attempt ended: %s %v; want a refusal naming the dead attempt", got.State, err)
	}
	cur, err := b.Load(ctx, e.ID)
	if err != nil || cur.State != FailedDefinite || cur.Dispatches != 1 {
		t.Fatalf("the refused retry changed the effect: %s %v dispatches=%d; want FAILED_DEFINITE", cur.State, err, cur.Dispatches)
	}
}
