package reducer

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"testing"

	"fabric/internal/domain"
)

// goodqa#15324 S1b reducer rules for ladder missions.

var allCommandKinds = []domain.CommandKind{
	domain.CmdAdmitMission, domain.CmdReviseIntent, domain.CmdStartAttempt, domain.CmdAckStart,
	domain.CmdRecordEvidence, domain.CmdFinishAttempt, domain.CmdExpireLease, domain.CmdReconcileAttempt,
	domain.CmdSelectCandidate, domain.CmdAccept, domain.CmdDeliver, domain.CmdWake, domain.CmdAckWake,
	domain.CmdSubmitEffect, domain.CmdEffectUnknown, domain.CmdRetryEffect, domain.CmdResolveEffect,
	domain.CmdRelease, domain.CmdRecordLiveTest, domain.CmdCompleteMission, domain.CmdUnpauseMission,
}

const ladderID domain.MissionID = "m_ladder-t1"

func sp(v string) *string { return &v }

func failRec(fp string) *domain.LadderRecord {
	return &domain.LadderRecord{Class: "FAIL", Step: "review", Fp: sp(fp), Rsha: sp(strings.Repeat("cd", 32)), Launched: true, Counted: true, SpendMicros: 1000}
}

func record(id domain.MissionID, r *domain.LadderRecord) domain.Command {
	return domain.Command{Kind: domain.CmdRecordLadderReceipt, Mission: id, Ladder: r}
}

func fpN(n int) string { return fmt.Sprintf("%064x", n) }

// Rule (a): a ladder mission refuses every command except RECORD_LADDER_RECEIPT.
// Each command is tried with generous env, so only the rule can refuse.
func TestLadderMissionRefusesEveryOtherCommand(t *testing.T) {
	m := domain.NewMission(ladderID)
	for _, k := range allCommandKinds {
		cmd := domain.Command{Kind: k, Mission: ladderID, Attempt: "r_1", Fence: 1, Host: "h_t2", Role: "writer",
			Path: domain.ProviderAnthropicAPI, Reason: "x", Token: "ask:a_1"}
		evs, err := Decide(m, cmd, Env{Cap: 99, Operator: true, Provider: domain.ProviderAnthropicAPI})
		if !errors.Is(err, domain.ErrLadderMission) || len(evs) != 0 {
			t.Errorf("%s on a ladder mission: got (%d events, %v), want ErrLadderMission and no events", k, len(evs), err)
		}
	}
	if evs, err := Decide(m, domain.Command{Kind: "SOMETHING_NEW", Mission: ladderID}, Env{Cap: 99}); !errors.Is(err, domain.ErrLadderMission) || len(evs) != 0 {
		t.Errorf("unknown command on a ladder mission: (%d, %v)", len(evs), err)
	}
}

// Rule (b): a record never lands on a real mission, in any state.
func TestRecordLadderReceiptRefusedOnNonLadderMission(t *testing.T) {
	for _, id := range []domain.MissionID{"m_real", "m_ladder", "m_ladderx", "m_a-ladder-x"} {
		m := domain.NewMission(id)
		evs, err := Decide(m, record(id, failRec(fpN(1))), Env{Cap: 99, Operator: true})
		if !errors.Is(err, domain.ErrNotLadderMission) || len(evs) != 0 {
			t.Errorf("%s: got (%d events, %v), want ErrNotLadderMission and no events", id, len(evs), err)
		}
	}
	m := domain.NewMission("m_real")
	evs, err := Decide(m, domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_real"}, Env{Cap: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		m = Apply(m, e)
	}
	if _, err := Decide(m, record("m_real", failRec(fpN(1))), Env{Cap: 1}); !errors.Is(err, domain.ErrNotLadderMission) {
		t.Errorf("admitted real mission accepted a record: %v", err)
	}
}

// Validation is the domain's; the reducer refuses and emits nothing.
func TestRecordLadderReceiptValidation(t *testing.T) {
	m := domain.NewMission(ladderID)
	for i, r := range []*domain.LadderRecord{nil, {Class: "MAYBE", Step: "review"}, {Class: "FAIL", Step: "review"},
		{Step: "approve_top"}} {
		evs, err := Decide(m, record(ladderID, r), Env{})
		if !errors.Is(err, domain.ErrInvalidReceipt) || len(evs) != 0 {
			t.Errorf("bad record %d: got (%d events, %v), want ErrInvalidReceipt and no events", i, len(evs), err)
		}
	}
}

// The event carries the record the caller classified, with the right kind; the
// mission neither holds a slot nor leaves IDLE. The stored record is a copy.
func TestRecordLadderReceiptEmitsEvent(t *testing.T) {
	cases := []struct {
		r    *domain.LadderRecord
		kind domain.EventKind
	}{
		{failRec(fpN(1)), domain.EvLadderReceipt},
		{&domain.LadderRecord{Class: "NOT_COUNTED", Step: "review", Rsha: sp(strings.Repeat("ab", 32))}, domain.EvLadderReceipt},
		{&domain.LadderRecord{Class: "CLAIM", Step: "claim", Commit: sp("abcdef0")}, domain.EvLadderReceipt},
		{&domain.LadderRecord{Class: "NOT_COUNTED", Step: "spend", SpendMicros: 5, Ordinal: 1}, domain.EvLadderReceipt},
		{&domain.LadderRecord{Step: "approve_top", Approval: &domain.LadderApproval{Ask: "a_1", Src: "user:s/u", By: "op:eric"}}, domain.EvLadderTopApproved},
	}
	for _, c := range cases {
		m := domain.NewMission(ladderID)
		evs, err := Decide(m, record(ladderID, c.r), Env{Cap: 0})
		if err != nil || len(evs) != 1 {
			t.Fatalf("%+v: (%d events, %v)", c.r, len(evs), err)
		}
		e := evs[0]
		if e.Kind != c.kind || e.Mission != ladderID || e.Ladder == nil || e.Ladder == c.r || e.Ladder.Step != c.r.Step || e.Ladder.Class != c.r.Class {
			t.Fatalf("%+v: event %+v, want kind %s and a copy of the record", c.r, e, c.kind)
		}
		m = Apply(m, e)
		if m.Execution != domain.ExecIdle || m.Execution.HoldsSlot() || m.Terminal != domain.TermNone {
			t.Fatalf("%+v: ladder mission moved to %s/%s", c.r, m.Execution, m.Terminal)
		}
	}
}

func TestLadderKindsAreInTheRegistry(t *testing.T) {
	for _, k := range []domain.EventKind{domain.EvLadderReceipt, domain.EvLadderTopApproved} {
		if !domain.KnownEventKind(k) {
			t.Errorf("%s not known", k)
		}
		// Replay of a log holding one must not stop at it.
		m, err := Replay(ladderID, []domain.Event{{Kind: k, Mission: ladderID, Seq: 1, Ladder: &domain.LadderRecord{Step: "x"}}})
		if err != nil || m.Seq != 1 {
			t.Errorf("%s: replay (%d, %v)", k, m.Seq, err)
		}
	}
}

// AC-1: replaying the log gives the live state, for a seeded random log that
// mixes valid records with refused ones (which add nothing to the log).
func TestLadderReplayEqualsLive(t *testing.T) {
	seed := int64(15324)
	if v := os.Getenv("LADDER_SEED"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		seed = n
	}
	t.Logf("seed=%d", seed)
	rng := rand.New(rand.NewSource(seed))
	m := domain.NewMission(ladderID)
	var log []domain.Event
	classes := []string{"FAIL", "PASS", "NOT_COUNTED", "MAYBE", ""}
	for i := 0; i < 300; i++ {
		r := &domain.LadderRecord{Class: classes[rng.Intn(len(classes))], Step: "review", Rsha: sp(fmt.Sprintf("%064x", i)), Launched: rng.Intn(5) != 0}
		if r.Class == "FAIL" {
			r.Fp = sp(fpN(rng.Intn(4)))
		}
		evs, err := Decide(m, record(ladderID, r), Env{})
		if err != nil {
			if !errors.Is(err, domain.ErrInvalidReceipt) {
				t.Fatalf("seed %d step %d: unexpected %v", seed, i, err)
			}
			continue
		}
		for _, e := range evs {
			e.Seq = m.Seq + 1
			m = Apply(m, e)
			log = append(log, e)
		}
	}
	if len(log) < 100 {
		t.Fatalf("seed %d: only %d valid records; the generator is vacuous", seed, len(log))
	}
	rebuilt, err := Replay(ladderID, log)
	if err != nil {
		t.Fatalf("seed %d: replay: %v", seed, err)
	}
	if a, b := Digest(rebuilt), Digest(m); a != b {
		t.Fatalf("seed %d: replay digest %s != live %s", seed, a, b)
	}
}
