package effects

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"fabric/internal/domain"
)

// Kimi effect-identity analysis, checked against fabric: these pin replay of
// a settled effect after its attempt ended, the whole business key on
// replay, the persisted request against its hash, and settle bound to the
// dispatch that produced the receipt.

func endAttempt(t *testing.T, ctx context.Context, b *Broker) {
	t.Helper()
	if _, err := b.st.Execute(ctx, "host", "fin-r1", domain.Command{Kind: domain.CmdFinishAttempt, Mission: "m_1", Attempt: "r_1",
		Fence: 1, Outcome: "wait", WaitKey: "job:id"}); err != nil {
		t.Fatal(err)
	}
}

func TestTerminalReplayAfterAttemptEnds(t *testing.T) {
	ctx, _, b, fake := setup(t)
	e, _ := b.Submit(ctx, fake, spec("ki1", "hello"))
	if e, err := b.Dispatch(ctx, fake, e.ID); err != nil || e.State != Confirmed {
		t.Fatalf("dispatch: %s %v", e.State, err)
	}
	endAttempt(t, ctx, b)
	again, err := b.Submit(ctx, fake, spec("ki1", "hello"))
	if err != nil || !again.Replayed || again.ID != e.ID || again.State != Confirmed {
		t.Fatalf("replay after the attempt ended: %+v %v", again, err)
	}
	if fake.Count() != 1 {
		t.Fatalf("remote writes %d, want 1", fake.Count())
	}
	// A new intent still needs a live fence.
	if _, err := b.Submit(ctx, fake, spec("ki1-new", "hello")); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("new intent on an ended attempt: %v", err)
	}
}

func TestReplayBindsWholeIntent(t *testing.T) {
	ctx, _, b, fake := setup(t)
	if _, err := b.Submit(ctx, fake, spec("ki2", "hello")); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(*IntentSpec){
		"mission": func(s *IntentSpec) { s.Mission = "m_other" },
		"subject": func(s *IntentSpec) { s.Subject = "ticket:goodqa#1" },
		"attempt": func(s *IntentSpec) { s.Attempt = "r_other" },
		"fence":   func(s *IntentSpec) { s.Fence = 2 },
		"request": func(s *IntentSpec) { s.Request = json.RawMessage(`{"ticket": "goodqa#0", "body": "changed"}`) },
		// Red-team F6: a rerun with --approve must not replay an unapproved intent.
		"approval": func(s *IntentSpec) { s.NeedsApproval = true },
	} {
		s := spec("ki2", "hello")
		mod(&s)
		if e, err := b.Submit(ctx, fake, s); !errors.Is(err, domain.ErrIdempotencyConflict) {
			t.Errorf("%s changed under the same key: want ErrIdempotencyConflict, got %+v %v", name, e, err)
		}
	}
}

func TestDispatchRefusesRequestThatNoLongerMatchesItsHash(t *testing.T) {
	ctx, st, b, fake := setup(t)
	e, _ := b.Submit(ctx, fake, spec("ki3", "approved text"))
	swap := `UPDATE effects SET request = '{"ticket": "goodqa#0", "body": "swapped"}' WHERE id = $1`
	// Review Q1 on 3f38cc3: the database refuses the swap for every role.
	if _, err := st.Pool().Exec(ctx, swap, e.ID); err == nil {
		t.Fatal("a request swap after insert was accepted")
	}
	// The hash check in Dispatch is the second layer: exercise it with the
	// guard off, which only the table owner can do.
	tx, err := st.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"ALTER TABLE effects DISABLE TRIGGER effects_zz_outcome_guard", swap, "ALTER TABLE effects ENABLE TRIGGER effects_zz_outcome_guard"} {
		if _, err := tx.Exec(ctx, q, func() []any {
			if q == swap {
				return []any{e.ID}
			}
			return nil
		}()...); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Dispatch(ctx, fake, e.ID); !errors.Is(err, ErrRequestTampered) {
		t.Fatalf("dispatch of a request that differs from its hash: want ErrRequestTampered, got %v", err)
	}
	if fake.Count() != 0 {
		t.Fatalf("remote writes %d, want 0", fake.Count())
	}
}

// A receipt settles only the dispatch it came from: if the row moved on to a
// later dispatch meanwhile (recovery, reconcile, retry), the stale receipt
// does not overwrite it.
func TestStaleReceiptDoesNotSettleLaterDispatch(t *testing.T) {
	ctx, st, b, fake := setup(t)
	e, _ := b.Submit(ctx, fake, spec("ki4", "hello"))
	b.afterTransport = func(e Effect, rc Receipt) {
		// The row is recovered, read back NOT_APPLIED, retried and dispatched again.
		for _, q := range []string{
			"UPDATE effects SET state = 'UNKNOWN' WHERE id = $1",
			"UPDATE effects SET state = 'RECONCILING' WHERE id = $1",
			"UPDATE effects SET state = 'NOT_APPLIED' WHERE id = $1",
			"UPDATE effects SET state = 'INTENT' WHERE id = $1",
			"UPDATE effects SET state = 'DISPATCHED', dispatches = dispatches + 1 WHERE id = $1",
		} {
			if _, err := st.Pool().Exec(context.Background(), q, e.ID); err != nil {
				t.Error(err)
			}
		}
	}
	got, err := b.Dispatch(ctx, fake, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != Dispatched || got.Dispatches != 2 {
		t.Fatalf("a receipt from dispatch 1 settled dispatch %d: state %s", got.Dispatches, got.State)
	}
}

// Red-team F3: a number that jsonb re-renders (1e2 as 100) would make the
// stored request disagree with its hash forever and burn the key. It is
// refused at intake, before anything is stored; the canonical form works.
func TestNonCanonicalNumberRefusedAtIntake(t *testing.T) {
	ctx, st, b, fake := setup(t)
	s := spec("ki5", "n")
	for _, lit := range []string{"1e2", "1E2", "-0", "-0.0", "2.5e-1"} {
		s.Request = json.RawMessage(`{"ticket": "goodqa#0", "body": "n", "n": ` + lit + `}`)
		if _, err := b.Submit(ctx, fake, s); !errors.Is(err, domain.ErrInvalidTransition) {
			t.Errorf("%s: want refused at intake, got %v", lit, err)
		}
	}
	var n int
	if err := st.Pool().QueryRow(ctx, "SELECT count(*) FROM effects WHERE idem_key = 'ki5'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("a refused request was stored: %d rows %v", n, err)
	}
	s.Request = json.RawMessage(`{"ticket": "goodqa#0", "body": "n", "n": 100, "x": 1.50, "y": -3}`)
	e, err := b.Submit(ctx, fake, s)
	if err != nil {
		t.Fatal(err)
	}
	if e, err = b.Dispatch(ctx, fake, e.ID); err != nil || e.State != Confirmed {
		t.Fatalf("canonical numbers did not dispatch: %s %v", e.State, err)
	}
}
