package sched

import (
	"context"
	"fmt"
	"testing"
	"time"

	"fabric/internal/domain"
)

// goodqa#15586 F19a-0 NEG-19: a burst of accepted Executes (each now pings the
// wake channel) must not cost one scheduler tick per command: Run coalesces the
// queued hints. The tick proxy is dispatchQueries (one per tick, provider_test).
func TestExecuteHintBurstCoalesced(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	st, ln, cfg := ownerSetup(t, "m_burst")
	cfg.NoDispatch = false
	cfg.Backoff = time.Second // keeps backstop timer ticks out of the quiet window
	s := New(st, cfg, ln)
	rctx, rcancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- s.Run(rctx) }()
	time.Sleep(500 * time.Millisecond) // let the first ticks settle
	start := time.Now()
	const n = 100
	for i := 0; i < n; i++ {
		cmd := domain.Command{Kind: domain.CmdReviseIntent, Mission: "m_burst", Reason: fmt.Sprintf("r%d", i)}
		if _, err := st.Execute(ctx, "chat", domain.CommandID(fmt.Sprintf("burst-%d", i)), cmd); err != nil {
			t.Fatalf("execute %d: %v", i, err)
		}
	}
	burstEnd := time.Now()
	time.Sleep(1500 * time.Millisecond)
	t1 := s.ticks.Load()
	time.Sleep(2 * time.Second)
	t2 := s.ticks.Load() // queued hints must not keep producing ticks after the burst
	rcancel()
	<-done
	total := s.ticks.Load()
	elapsed := time.Since(start)
	gap := 100 * time.Millisecond
	t.Logf("ticks: %d in %s for a burst of %d accepted Executes (%d in the quiet 2 s after the burst); burst took %s", total, elapsed.Round(time.Millisecond), n, t2-t1, burstEnd.Sub(start).Round(time.Millisecond))
	// Control (the spacing removed): 118 to 122 ticks inside the burst alone. A variant
	// with the spacing but without the drain stops short in this window yet keeps ticking
	// once per queued hint afterwards, which the quiet window below catches.
	if limit := int(elapsed/gap) + 10; int(total) > limit {
		t.Fatalf("%d scheduler ticks in %s for %d commands (limit %d): the hints are not coalesced", total, elapsed, n, limit)
	}
	if t2-t1 > 3 {
		t.Fatalf("%d ticks in the 2 s after the burst ended: queued hints are not coalesced", t2-t1)
	}
	close(ln.block)
}

// goodqa#15586 F19a-0: a hint-triggered wake is counted durably (shown by
// `fab metrics`), a timer tick is not, so the counter proves the wake path.
func TestWakeHintCounter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	st, ln, cfg := ownerSetup(t, "m_wakectr")
	cfg.NoDispatch = false
	cfg.Backoff = 20 * time.Millisecond // many timer ticks fall in the idle window
	cfg.MaxBackoff = 40 * time.Millisecond
	if _, err := st.Pool().Exec(ctx, `DELETE FROM sched_counters WHERE name = 'wake_hints_total'`); err != nil {
		t.Fatal(err)
	}
	count := func() int64 {
		var v int64
		err := st.Pool().QueryRow(ctx, `SELECT COALESCE((SELECT value FROM sched_counters WHERE name = 'wake_hints_total'), 0)`).Scan(&v)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	s := New(st, cfg, ln)
	rctx, rcancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- s.Run(rctx) }()
	time.Sleep(time.Second)
	// The first ticks may themselves commit commands (hinting); take the baseline
	// after they settle and require it to stay put while idle (no timer count).
	base := count()
	tk0 := s.ticks.Load()
	time.Sleep(time.Second)
	if n := s.ticks.Load() - tk0; n < 10 {
		t.Fatalf("only %d timer ticks in the idle second: the window does not exercise timer ticks", n)
	}
	if c := count(); c != base {
		t.Fatalf("wake_hints_total moved %d -> %d across timer ticks while idle: only a hint may count", base, c)
	}
	cmd := domain.Command{Kind: domain.CmdReviseIntent, Mission: "m_wakectr", Reason: "wake"}
	if _, err := st.Execute(ctx, "chat", "wakectr-1", cmd); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for count() == base && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if c := count(); c <= base {
		t.Fatalf("wake_hints_total = %d (baseline %d) 2 s after an accepted command: the hint did not wake the scheduler", c, base)
	}
	rcancel()
	<-done
	close(ln.block)
}
