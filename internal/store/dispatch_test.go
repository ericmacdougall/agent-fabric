package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"fabric/internal/domain"
)

// C5 (0012): the final dispatch grant. Ported by intent from Kimi's
// TestAuthorizeDispatch* tests, with DB time read after the locks (F3).

// acked starts an attempt with a hold on res and ACKs it, so it is RUNNING.
func acked(t *testing.T, st *Store, m domain.MissionID, a domain.AttemptID, res string, ttl time.Duration) int64 {
	t.Helper()
	ctx := context.Background()
	f := liveAttempt(t, st, ctx, m, a)
	if _, err := hold(st, ctx, "hold:"+string(a), a, f, ttl, item(res, 1)); err != nil {
		t.Fatal(err)
	}
	ok(t, st, ctx, "fabd", "ack:"+string(a), domain.Command{Kind: domain.CmdAckStart, Mission: m, Attempt: a, Fence: f})
	return f
}

func grantRows(t *testing.T, st *Store, a domain.AttemptID) int {
	t.Helper()
	var n int
	if err := st.pool.QueryRow(context.Background(), "SELECT count(*) FROM dispatch_grants WHERE attempt_id = $1", string(a)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAuthorizeDispatchGrantsOnceAndReplays(t *testing.T) {
	st, ctx := openTest(t)
	capacity(t, st, ctx, "slot.g", 2)
	f := acked(t, st, "m_dg1", "r_dg1", "slot.g", time.Minute)
	g, err := st.AuthorizeDispatch(ctx, "r_dg1", f, []string{"slot.g"})
	if err != nil || !g.Granted || g.Replayed || len(g.Reservations) != 1 {
		t.Fatalf("first: %+v %v", g, err)
	}
	again, err := st.AuthorizeDispatch(ctx, "r_dg1", f, []string{"slot.g"})
	if err != nil || !again.Granted || !again.Replayed || again.Reservations[0] != g.Reservations[0] || !again.DecidedAt.Equal(g.DecidedAt) {
		t.Fatalf("replay: %+v %v (first %+v)", again, err, g)
	}
	if n := grantRows(t, st, "r_dg1"); n != 1 {
		t.Fatalf("%d grant rows, want 1", n)
	}
	// Stale caller: an older fence is refused and nothing new is stored.
	if g, err := st.AuthorizeDispatch(ctx, "r_dg1", f+1, []string{"slot.g"}); !errors.Is(err, domain.ErrStaleFence) {
		t.Fatalf("other fence: want ErrStaleFence, got %+v %v", g, err)
	}
	// The stored decision cannot be edited or removed.
	for _, q := range []string{"UPDATE dispatch_grants SET outcome = 'DENIED', reason = 'x'", "DELETE FROM dispatch_grants", "TRUNCATE dispatch_grants"} {
		var pgErr *pgconn.PgError
		if _, err := st.pool.Exec(ctx, q); !errors.As(err, &pgErr) || (pgErr.Code != "23001") {
			t.Errorf("%s: want 23001, got %v", q, err)
		}
	}
}

func TestAuthorizeDispatchDenials(t *testing.T) {
	st, ctx := openTest(t)
	capacity(t, st, ctx, "slot.d", 10)
	if _, err := st.pool.Exec(ctx, "UPDATE slots SET cap = 8 WHERE id = 1"); err != nil { // six missions here
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		setup  func(t *testing.T) (domain.AttemptID, int64)
		need   []string
		reason string
	}{
		{"not acked", func(t *testing.T) (domain.AttemptID, int64) {
			f := liveAttempt(t, st, ctx, "m_dd1", "r_dd1")
			if _, err := hold(st, ctx, "h-dd1", "r_dd1", f, time.Minute, item("slot.d", 1)); err != nil {
				t.Fatal(err)
			}
			return "r_dd1", f
		}, []string{"slot.d"}, "attempt is LEASED, not RUNNING"},
		{"no hold for a required resource", func(t *testing.T) (domain.AttemptID, int64) {
			return "r_dd2", acked(t, st, "m_dd2", "r_dd2", "slot.d", time.Minute)
		}, []string{"slot.d", "slot.other"}, "no held reservation for slot.other"},
		{"hold released before dispatch", func(t *testing.T) (domain.AttemptID, int64) {
			f := acked(t, st, "m_dd3", "r_dd3", "slot.d", time.Minute)
			rs, _ := st.Reservations(ctx, "r_dd3")
			if _, err := st.Release(ctx, rs[0].ID); err != nil {
				t.Fatal(err)
			}
			return "r_dd3", f
		}, []string{"slot.d"}, "no held reservation for slot.d"},
		{"lease expired", func(t *testing.T) (domain.AttemptID, int64) {
			f := acked(t, st, "m_dd4", "r_dd4", "slot.d", time.Minute)
			if _, err := st.pool.Exec(ctx, "UPDATE attempts SET lease_until = now() - interval '1 second' WHERE id = 'r_dd4'"); err != nil {
				t.Fatal(err)
			}
			return "r_dd4", f
		}, []string{"slot.d"}, "attempt lease expired"},
		{"intent revised after start", func(t *testing.T) (domain.AttemptID, int64) {
			f := acked(t, st, "m_dd5", "r_dd5", "slot.d", time.Minute)
			ok(t, st, ctx, "chat", "rev-dd5", domain.Command{Kind: domain.CmdReviseIntent, Mission: "m_dd5", Reason: "scope change"})
			return "r_dd5", f
		}, []string{"slot.d"}, "intent revised: attempt at revision 0, mission at 1"},
		{"nothing required", func(t *testing.T) (domain.AttemptID, int64) {
			return "r_dd6", acked(t, st, "m_dd6", "r_dd6", "slot.d", time.Minute)
		}, nil, "no resources required"},
	}
	for _, c := range cases {
		a, f := c.setup(t)
		g, err := st.AuthorizeDispatch(ctx, a, f, c.need)
		if err != nil || g.Granted || g.Reason != c.reason {
			t.Errorf("%s: want denied %q, got %+v %v", c.name, c.reason, g, err)
			continue
		}
		if again, _ := st.AuthorizeDispatch(ctx, a, f, c.need); !again.Replayed || again.Granted || again.Reason != c.reason {
			t.Errorf("%s: the denial did not replay: %+v", c.name, again)
		}
	}
}

// DB time after the locks: a hold that is unexpired when the transaction
// begins but expired by the time the locks are held is denied. With now()
// (transaction start) it would be granted.
func TestAuthorizeDispatchUsesDBTimeAfterLocks(t *testing.T) {
	st, ctx := openTest(t)
	capacity(t, st, ctx, "slot.t", 2)
	f := acked(t, st, "m_dt1", "r_dt1", "slot.t", MinHoldTTL)
	rs, _ := st.Reservations(ctx, "r_dt1")
	st.hooks.grantLocked = func() {
		var past bool
		for !past {
			if err := st.pool.QueryRow(ctx, "SELECT clock_timestamp() > $1", rs[0].ExpiresAt).Scan(&past); err != nil {
				t.Error(err)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	defer func() { st.hooks.grantLocked = nil }()
	g, err := st.AuthorizeDispatch(ctx, "r_dt1", f, []string{"slot.t"})
	if err != nil || g.Granted || g.Reason != "reservation for slot.t expired" {
		t.Fatalf("expired by DB time after the locks: want denied, got %+v %v", g, err)
	}
}
