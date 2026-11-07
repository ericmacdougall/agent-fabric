package sched

import (
	"context"
	"sync/atomic"
	"testing"

	"fabric/internal/domain"
	"fabric/internal/store"
)

// goodqa#15082 T7: with the dispatch gate off, a run still gets a final
// admission check after ACK: a revised intent or an expired lease between
// ACK and Start launches nothing and abandons the attempt before launch.
func TestUngatedFinalAdmission(t *testing.T) {
	cases := []struct {
		name string
		cut  func(t *testing.T, st *store.Store, id domain.MissionID, a string)
	}{
		{"intent revised after ack", func(t *testing.T, st *store.Store, id domain.MissionID, a string) {
			if _, err := st.Execute(context.Background(), "chat", domain.CommandID("rev-"+string(id)), domain.Command{Kind: domain.CmdReviseIntent, Mission: id, Reason: "scope"}); err != nil {
				t.Fatal(err)
			}
		}},
		{"lease expired after ack", func(t *testing.T, st *store.Store, id domain.MissionID, a string) {
			if _, err := st.Pool().Exec(context.Background(), `UPDATE attempts SET lease_until = clock_timestamp() - interval '1 second' WHERE id = $1`, a); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for i, c := range cases {
		id := domain.MissionID("m_fa" + string(rune('1'+i)))
		a := "r_fa" + string(rune('1'+i)) + "_1"
		st, ln, cfg := ownerSetup(t, id)
		s := New(st, cfg, ln)
		if s.gated() {
			t.Fatal("setup is gated; this test is about the ungated path")
		}
		cut, ran := c.cut, false
		s.afterAck = func() { ran = true; cut(t, st, id, a) }
		if err := s.runOnce(context.Background(), id); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !ran {
			t.Errorf("%s: the after-ACK cutpoint never ran", c.name)
		}
		if n := atomic.LoadInt32(&ln.starts); n != 0 {
			t.Errorf("%s: %d starts, want 0", c.name, n)
		}
		if got := attemptState(t, st, a); got != string(domain.AttemptFailedDefinite) {
			t.Errorf("%s: attempt %s, want FAILED_DEFINITE", c.name, got)
		}
	}
}
