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
	s := New(st, cfg, ln)
	rctx, rcancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- s.Run(rctx) }()
	time.Sleep(500 * time.Millisecond) // let the first ticks settle
	const n = 100
	for i := 0; i < n; i++ {
		cmd := domain.Command{Kind: domain.CmdReviseIntent, Mission: "m_burst", Reason: fmt.Sprintf("r%d", i)}
		if _, err := st.Execute(ctx, "chat", domain.CommandID(fmt.Sprintf("burst-%d", i)), cmd); err != nil {
			t.Fatalf("execute %d: %v", i, err)
		}
	}
	time.Sleep(1500 * time.Millisecond)
	rcancel()
	<-done
	ticks := s.dispatchQueries // read after Run returned; includes the few settle ticks before the burst
	t.Logf("ticks during a burst of %d accepted Executes: %d", n, ticks)
	// Control (the gap removed): 118 to 122 ticks. With the 100 ms gap the whole
	// 2.5 s window holds at most about 25 hint ticks plus the settle ticks.
	if ticks > 45 {
		t.Fatalf("%d scheduler ticks for %d commands: the hints are not coalesced", ticks, n)
	}
	close(ln.block)
}
