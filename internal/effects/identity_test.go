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
	if _, err := st.Pool().Exec(ctx, `UPDATE effects SET request = '{"ticket": "goodqa#0", "body": "swapped"}' WHERE id = $1`, e.ID); err != nil {
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
		if _, err := st.Pool().Exec(context.Background(), `UPDATE effects SET dispatches = dispatches + 1 WHERE id = $1`, e.ID); err != nil {
			t.Error(err)
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
