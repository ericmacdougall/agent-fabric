package projection

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"fabric/internal/domain"
	"fabric/internal/reducer"
)

// A long trajectory: 800 turns of checkpoint/evidence/effect noise with one
// UNKNOWN effect early on. The projection must stay under the cap, keep the
// UNKNOWN effect, report what it dropped and digest deterministically (AC-18,
// the N4 shape).
func longMission(t *testing.T, turns int) (domain.Mission, []domain.Event) {
	t.Helper()
	m := domain.NewMission("m_long")
	var log []domain.Event
	step := func(cmd domain.Command) {
		evs, err := reducer.Decide(m, cmd, reducer.Env{Cap: 4})
		if err != nil {
			t.Fatalf("%s: %v", cmd.Kind, err)
		}
		for i := range evs {
			evs[i].Seq = m.Seq + 1
			m = reducer.Apply(m, evs[i])
			log = append(log, evs[i])
		}
	}
	step(domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_long"})
	step(domain.Command{Kind: domain.CmdSelectCandidate, Mission: "m_long", Head: "sha_1"})
	fence := int64(0)
	for i := 0; i < turns; i++ {
		a := domain.AttemptID(fmt.Sprintf("r_%d", i))
		step(domain.Command{Kind: domain.CmdStartAttempt, Mission: "m_long", Attempt: a, Host: "h_t2", Role: "writer"})
		fence++
		step(domain.Command{Kind: domain.CmdAckStart, Mission: "m_long", Attempt: a, Fence: fence})
		step(domain.Command{Kind: domain.CmdRecordEvidence, Mission: "m_long", Attempt: a, Fence: fence, Revision: 0, Generation: 1, Head: "sha_1",
			Receipt: fmt.Sprintf("rcpt_%d_%s", i, strings.Repeat("x", 200))})
		if i == 0 {
			step(domain.Command{Kind: domain.CmdSubmitEffect, Mission: "m_long", Attempt: a, Fence: fence, EffectKind: "push", Request: []byte(`{"head":"sha_1"}`)})
			for id := range m.Effects {
				step(domain.Command{Kind: domain.CmdEffectUnknown, Mission: "m_long", Effect: id, Reason: "lost response"})
			}
		}
		step(domain.Command{Kind: domain.CmdFinishAttempt, Mission: "m_long", Attempt: a, Fence: fence, Outcome: "checkpoint", Reason: strings.Repeat("log line ", 40)})
		step(domain.Command{Kind: domain.CmdReconcileAttempt, Mission: "m_long", Attempt: a, Fence: fence, Result: "VERIFIED"})
	}
	return m, log
}

func TestLongTrajectoryStaysUnderCap(t *testing.T) {
	for _, turns := range []int{1, 10, 100, 400, 800} {
		m, log := longMission(t, turns)
		p, err := Build(m, log, DefaultCapTokens, "continue")
		if err != nil {
			t.Fatalf("turns=%d: %v", turns, err)
		}
		if p.Tokens > DefaultCapTokens {
			t.Fatalf("turns=%d: %d tokens over cap", turns, p.Tokens)
		}
		if len(p.Checkpoint.Unknown) != 1 {
			t.Fatalf("turns=%d: UNKNOWN effect not retained: %+v", turns, p.Checkpoint.Unknown)
		}
		if turns >= 100 && p.Dropped == 0 {
			t.Fatalf("turns=%d: nothing dropped from %d events", turns, len(log))
		}
		p2, _ := Build(m, log, DefaultCapTokens, "continue")
		if p.Digest != p2.Digest {
			t.Fatalf("turns=%d: digest not deterministic", turns)
		}
		t.Logf("turns=%d events=%d tokens=%d dropped=%d window=%d", turns, len(log), p.Tokens, p.Dropped, len(p.Window))
	}
}

func TestCheckpointOverBudgetFailsClosed(t *testing.T) {
	m := domain.NewMission("m_1")
	m.WaitKey = strings.Repeat("k", CheckpointMaxBytes+10)
	if _, err := Build(m, nil, DefaultCapTokens, ""); !errors.Is(err, ErrBudget) {
		t.Fatalf("oversized checkpoint: want ErrBudget, got %v", err)
	}
	m = domain.NewMission("m_1")
	if _, err := Build(m, nil, 10, ""); !errors.Is(err, ErrBudget) {
		t.Fatalf("cap below the checkpoint: want ErrBudget, got %v", err)
	}
}

func TestWindowIsChronologicalAndNewestKept(t *testing.T) {
	m, log := longMission(t, 50)
	p, err := Build(m, log, 400, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Window) == 0 || !strings.HasPrefix(p.Window[len(p.Window)-1], fmt.Sprintf("#%d ", log[len(log)-1].Seq)) {
		t.Fatalf("window does not end with the newest event: %v", p.Window)
	}
	prev := 0
	for _, line := range p.Window {
		var seq int
		if _, err := fmt.Sscanf(line, "#%d ", &seq); err != nil || seq <= prev {
			t.Fatalf("window not chronological at %q (prev %d, err %v)", line, prev, err)
		}
		prev = seq
	}
}
