package store

import (
	"errors"
	"testing"

	"fabric/internal/domain"
)

// NEG-19 (spec v4.4), review a68b6eddf1705c0e4: the unknown-kind check in
// Execute still refuses a mission that holds a foreign kind, for the abandon
// that is exempt from the held refusal as for any other command. The exemption
// is only from the held check.
func TestHeldAbandonStillRefusedOnUnknownKind(t *testing.T) {
	st, ctx := openTest(t)
	mk(t, st, ctx, "m_au")
	ok(t, st, ctx, "chat", "admit", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_au"})
	fence := startAttempt(t, st, ctx, "m_au", "r_au")
	plantBad(t, st, ctx, "m_au", 1000, "NEW_FUTURE_KIND", `{"kind":"NEW_FUTURE_KIND","mission":"m_au","seq":1000}`)
	abandon := domain.Command{Kind: domain.CmdFinishAttempt, Mission: "m_au", Attempt: "r_au", Fence: fence, Outcome: "abandoned", Prelaunch: true}
	for _, held := range []bool{false, true} {
		if held {
			holdIt(t, st, ctx, "m_au")
		}
		if _, err := st.Execute(ctx, "fabd", "finish:au", abandon); !errors.Is(err, domain.ErrUnsupportedEvent) {
			t.Fatalf("held=%v: the abandon of a mission holding an unknown kind gave %v, want ErrUnsupportedEvent", held, err)
		}
	}
	var state string
	if err := st.pool.QueryRow(ctx, `SELECT state FROM attempts WHERE id = 'r_au'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state == string(domain.AttemptFailedDefinite) || state == "FAILED_DEFINITE" {
		t.Fatalf("the refused abandon changed the attempt to %s", state)
	}
}
