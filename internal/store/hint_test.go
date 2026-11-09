package store

import (
	"context"
	"testing"
	"time"

	"fabric/internal/domain"
)

// goodqa#15586 F19a-0: every accepted Execute pings the wake channel after its
// commit, with no payload; a refusal and an idempotent replay do not.
func TestExecuteSendsWakeHint(t *testing.T) {
	st, ctx := openTest(t)
	mk(t, st, ctx, "m_hint")
	conn, err := st.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN "+WakeChannel); err != nil {
		t.Fatal(err)
	}
	drain := func() {
		for {
			dctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
			_, err := conn.Conn().WaitForNotification(dctx)
			cancel()
			if err != nil {
				return
			}
		}
	}
	got := func(wait time.Duration) (string, bool) {
		wctx, cancel := context.WithTimeout(ctx, wait)
		defer cancel()
		n, err := conn.Conn().WaitForNotification(wctx)
		if err != nil {
			return "", false
		}
		return n.Payload, true
	}
	drain()

	admit := domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_hint"}
	if _, err := st.Execute(ctx, "chat", "hint-admit", admit); err != nil {
		t.Fatal(err)
	}
	if payload, ok := got(2 * time.Second); !ok || payload != "" {
		t.Fatalf("no content-free hint after an accepted command (got %v, payload %q)", ok, payload)
	}
	drain()
	// An idempotent replay stores nothing: no hint.
	if _, err := st.Execute(ctx, "chat", "hint-admit", admit); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if _, ok := got(400 * time.Millisecond); ok {
		t.Fatal("a replay sent a hint")
	}
	// A refused command stores no event: no hint.
	if _, err := st.Execute(ctx, "chat", "hint-admit-2", admit); err == nil {
		t.Fatal("a second admit should be refused")
	}
	if _, ok := got(400 * time.Millisecond); ok {
		t.Fatal("a refused command sent a hint")
	}
}
