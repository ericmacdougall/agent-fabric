package effects

import (
	"context"
	"sync/atomic"
	"testing"

	"fabric/internal/m1build"
	"fabric/internal/store"
)

// legacySkip is the reason a legacy test stops in an M1 build.
const legacySkip = "legacy non-canonical ticket_comment contract does not exist in an M1 build; M1 refusals asserted (goodqa#15966)"

// legacyTicketComment guards a test of the legacy ticket_comment contract
// (a non-canonical key on an unmarked database). Untagged it does nothing.
// In an M1 build that contract does not exist, so it asserts what the M1
// build does instead, on the mission and attempt setup() created, and then
// skips the legacy assertions:
//   - unmarked: Submit is refused m1_policy_marker_missing, no row is
//     written and the executor is never called past Validate;
//   - marked m1: the same Submit is refused m1_noncanonical_key, with no row
//     and no executor call.
func legacyTicketComment(t *testing.T, ctx context.Context) {
	t.Helper()
	if !m1build.Enabled {
		return
	}
	st, err := store.Open(ctx, testDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	b := New(st)
	s := spec("k-legacy-m1-probe", "legacy")
	if s.IdemKey == M1Key(s.Mission) {
		t.Fatal("probe key must be non-canonical")
	}
	ex := &callCounter{Executor: NewFake()}
	rows := func() int {
		var n int
		if err := st.Pool().QueryRow(ctx, `SELECT count(*) FROM effects WHERE idem_key = $1`, s.IdemKey).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	var on int
	if err := st.Pool().QueryRow(ctx, `SELECT count(*) FROM fabric_settings WHERE key = 'comment_policy'`).Scan(&on); err != nil || on != 0 {
		t.Fatalf("legacy test expects an unmarked database: %d marker rows %v", on, err)
	}
	if _, err := b.Submit(ctx, ex, s); !IsPolicyRefusal(err, CodeMarkerMissing) {
		t.Fatalf("M1 build, unmarked: want %s, got %v", CodeMarkerMissing, err)
	}
	if n, c := rows(), ex.calls.Load(); n != 0 || c != 0 {
		t.Fatalf("M1 build, unmarked: %d rows, %d executor calls", n, c)
	}
	setMarker(t, ctx, st, PolicyMarkerM1)
	if _, err := b.Submit(ctx, ex, s); !IsPolicyRefusal(err, CodeNonCanonical) {
		t.Fatalf("M1 build, marked: want %s, got %v", CodeNonCanonical, err)
	}
	if n, c := rows(), ex.calls.Load(); n != 0 || c != 0 {
		t.Fatalf("M1 build, marked: %d rows, %d executor calls", n, c)
	}
	t.Skip(legacySkip)
}

// callCounter counts every executor call that could reach the remote.
type callCounter struct {
	Executor
	calls atomic.Int64
}

func (c *callCounter) Precondition(ctx context.Context, e Effect) (bool, string, error) {
	c.calls.Add(1)
	return c.Executor.Precondition(ctx, e)
}

func (c *callCounter) Dispatch(ctx context.Context, sub Submission) (Receipt, error) {
	c.calls.Add(1)
	return c.Executor.Dispatch(ctx, sub)
}

func (c *callCounter) Reconcile(ctx context.Context, e Effect) (Observation, string, error) {
	c.calls.Add(1)
	return c.Executor.Reconcile(ctx, e)
}

// nonM1Broker is a broker that decides as a non-M1 build in either test
// build: it stands for a legacy broker that wrote a row before the marker
// existed. It sets the unexported field inside this test file only; the
// production API has no such switch.
func nonM1Broker(st *store.Store) *Broker {
	b := New(st)
	b.m1 = false
	return b
}
