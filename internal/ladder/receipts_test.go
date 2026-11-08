package ladder

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"fabric/internal/domain"
)

// goodqa#15324 S1b: the fold from stored ladder events into the unchanged
// Replay/Status, a golden oracle generated from escalation.py, and the cap.

func sp(v string) *string { return &v }

func hx(n int) string { return fmt.Sprintf("%064x", n) }

func ev(r domain.LadderRecord) domain.Event {
	k := domain.LadderEventKind(&r)
	return domain.Event{Kind: k, Mission: "m_ladder-t1", Ladder: &r}
}

func failEv(fp int, head string) domain.Event {
	r := domain.LadderRecord{Class: "FAIL", Step: "review", Fp: sp(hx(fp)), Rsha: sp(hx(1000 + fp)), Launched: true}
	if head != "" {
		r.Commit = sp(head)
	}
	return ev(r)
}

func ncEv() domain.Event {
	return ev(domain.LadderRecord{Class: "NOT_COUNTED", Step: "review", Rsha: sp(hx(5000))})
}

func claimEv(head string) domain.Event {
	return ev(domain.LadderRecord{Class: "CLAIM", Step: "claim", Commit: sp(head)})
}

const h1, h2 = "abcdef0123456789abcdef0123456789abcdef01", "0123456789abcdef0123456789abcdef01234567"

func TestFoldMapsFingerprintAndHead(t *testing.T) {
	got, err := FromEvents([]domain.Event{failEv(1, h1), claimEv(h1), ncEv()})
	if err != nil || len(got) != 3 {
		t.Fatalf("(%d, %v)", len(got), err)
	}
	if got[0].Class != "FAIL" || got[0].Fingerprint == nil || *got[0].Fingerprint != hx(1) || *got[0].Head != h1 {
		t.Fatalf("FAIL mapped wrong: %+v", got[0])
	}
	if got[1].Class != "CLAIM" || got[1].Fingerprint != nil || *got[1].Head != h1 {
		t.Fatalf("CLAIM mapped wrong: %+v", got[1])
	}
	if got[2].Class != "NOT_COUNTED" || got[2].Fingerprint != nil || got[2].Head != nil {
		t.Fatalf("NOT_COUNTED mapped wrong: %+v", got[2])
	}
}

func TestFoldRefusesForeignOrMalformedEvents(t *testing.T) {
	for name, e := range map[string]domain.Event{
		"other kind":    {Kind: domain.EvMissionAdmitted, Mission: "m_ladder-t1"},
		"no payload":    {Kind: domain.EvLadderReceipt, Mission: "m_ladder-t1"},
		"unknown class": {Kind: domain.EvLadderReceipt, Mission: "m_ladder-t1", Ladder: &domain.LadderRecord{Class: "MAYBE", Step: "review"}},
		"approval in a receipt event": {Kind: domain.EvLadderReceipt, Mission: "m_ladder-t1",
			Ladder: &domain.LadderRecord{Step: "approve_top", Approval: &domain.LadderApproval{Ask: "a", Src: "user:x", By: "op:e"}}},
	} {
		if _, err := Fold([]domain.Event{e}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// NEG-1 and NEG-2 through the real Replay: NOT_COUNTED changes nothing; the
// same fingerprint at the same head with no claimed fix is one failure; a new
// fingerprint resets the count.
func TestFoldCountsLikeEscalation(t *testing.T) {
	p := testPolicy() // failures_per_rung 2
	fold := func(evs ...domain.Event) Computed {
		t.Helper()
		l, err := FromEvents(evs)
		if err != nil {
			t.Fatal(err)
		}
		c, err := Replay(p, l)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	if c := fold(ncEv(), ncEv(), ncEv(), ncEv()); c.Rung != "R0" || c.FailuresAtRung != 0 {
		t.Fatalf("NEG-1: uncounted receipts counted: %+v", c)
	}
	if c := fold(failEv(1, h1), failEv(1, h1)); c.Rung != "R0" || c.FailuresAtRung != 1 {
		t.Fatalf("NEG-2: same fingerprint at the same head counted twice: %+v", c)
	}
	if c := fold(failEv(1, h1), failEv(2, h2)); c.Rung != "R0" || c.FailuresAtRung != 1 {
		t.Fatalf("NEG-2: a new fingerprint did not reset the count: %+v", c)
	}
	if c := fold(failEv(1, h1), claimEv(h1), failEv(1, h1)); c.Rung != "R1" || c.Climbs != 1 {
		t.Fatalf("a failure after a claimed fix did not count: %+v", c)
	}
}

// PL2-3: the cap counts LADDER_RECEIPT only. 4096 pass; 4097 are refused with
// no events returned; approvals do not use up receipt slots.
func TestFoldCapCountsReceiptsOnly(t *testing.T) {
	if MaxReceiptsPerTask != 4096 {
		t.Fatalf("cap drifted to %d", MaxReceiptsPerTask)
	}
	mk := func(n int) []domain.Event {
		out := make([]domain.Event, n)
		for i := range out {
			out[i] = failEv(i%7, "")
		}
		return out
	}
	approval := ev(domain.LadderRecord{Step: "approve_top", Approval: &domain.LadderApproval{Ask: "a_1", Src: "user:s/u", By: "op:eric"}})
	got, err := FromEvents(append(mk(MaxReceiptsPerTask-1), approval))
	if err != nil || len(got) != MaxReceiptsPerTask-1 {
		t.Fatalf("cap-1 receipts + 1 approval: (%d, %v)", len(got), err)
	}
	got, err = FromEvents(append(mk(MaxReceiptsPerTask-1), approval, failEv(1, "")))
	if err != nil || len(got) != MaxReceiptsPerTask {
		t.Fatalf("exactly the cap with an approval between: (%d, %v)", len(got), err)
	}
	got, err = FromEvents(mk(MaxReceiptsPerTask + 1))
	if !errors.Is(err, domain.ErrTooManyReceipts) || got != nil || !strings.Contains(err.Error(), "4096") {
		t.Fatalf("one past the cap: (%d events, %v), want ErrTooManyReceipts naming 4096 and no events", len(got), err)
	}
}

func TestFoldApprovalBound(t *testing.T) {
	a := ev(domain.LadderRecord{Step: "approve_top", Approval: &domain.LadderApproval{Ask: "a_1", Src: "user:s/u", By: "op:eric"}})
	var evs []domain.Event
	for i := 0; i < domain.MaxLadderApprovals+1; i++ {
		evs = append(evs, a)
	}
	if _, err := Fold(evs); !errors.Is(err, domain.ErrTooManyReceipts) {
		t.Fatalf("approvals over the bound: %v", err)
	}
}

// NEG-3: a rung that needs user permission is STOP_AND_ASK until an approval
// exists for that task; the decision is advisory.
func TestDecisionStopAndAskUntilApproved(t *testing.T) {
	p := testPolicy()
	// R3 needs user permission in testPolicy; failures_per_rung 2 climbs one rung per 2 distinct failures.
	var evs []domain.Event
	for i := 0; i < 6; i++ {
		evs = append(evs, failEv(i, ""))
		evs = append(evs, failEv(i, strings.Repeat("a", 7+i)))
	}
	s, err := StatusOf(p, "t1", evs)
	if err != nil {
		t.Fatal(err)
	}
	if !s.NeedsUserPermission || s.Rung != "R3" || s.Decision != "STOP_AND_ASK" || !s.Advisory || s.Approved {
		t.Fatalf("at the permission rung without approval: %+v", s)
	}
	approval := ev(domain.LadderRecord{Step: "approve_top", Approval: &domain.LadderApproval{Ask: "a_1", Src: "user:s/u", By: "op:eric"}})
	s, err = StatusOf(p, "t1", append(evs, approval))
	if err != nil || s.Decision != "CONTINUE" || !s.Approved || !s.Advisory {
		t.Fatalf("with approval: (%+v, %v)", s, err)
	}
	if s, err = StatusOf(p, "t1", nil); err != nil || s.Decision != "CONTINUE" || s.Rung != "R0" {
		t.Fatalf("empty log: (%+v, %v)", s, err)
	}
}

// PL2-1 (control in Go): two identical spends both count.
func TestFoldSumsIdenticalSpends(t *testing.T) {
	sp1 := ev(domain.LadderRecord{Class: "NOT_COUNTED", Step: "spend", SpendMicros: 500_000, Ordinal: 1})
	sp2 := ev(domain.LadderRecord{Class: "NOT_COUNTED", Step: "spend", SpendMicros: 500_000, Ordinal: 2})
	f, err := Fold([]domain.Event{sp1, sp2})
	if err != nil || f.SpendMicros != 1_000_000 {
		t.Fatalf("(%+v, %v), want 1.0 USD", f, err)
	}
	if MicrosText(1_000_000) != "1.000000" || MicrosText(556_500_000) != "556.500000" || MicrosText(5) != "0.000005" {
		t.Fatalf("MicrosText: %s %s %s", MicrosText(1_000_000), MicrosText(556_500_000), MicrosText(5))
	}
}

// CI-2 (S1B-A2): escalation.py MAX_EVENTS counts events only and add_spend adds
// none, so spend records never use up receipt slots; they have their own bound.
func TestFoldSpendsDoNotCountTowardTheReceiptCap(t *testing.T) {
	var evs []domain.Event
	for i := 0; i < MaxReceiptsPerTask-1; i++ {
		evs = append(evs, failEv(i%7, ""))
	}
	for i := 1; i <= 500; i++ {
		evs = append(evs, ev(domain.LadderRecord{Class: "NOT_COUNTED", Step: "spend", SpendMicros: 1000, Ordinal: int64(i)}))
	}
	evs = append(evs, failEv(1, "")) // receipt number 4096 after 500 spends: still accepted
	f, err := Fold(evs)
	if err != nil || f.Receipts != MaxReceiptsPerTask || f.SpendMicros != 500*1000 {
		t.Fatalf("cap-1 receipts + 500 spends + 1 receipt: (receipts %d, spend %d, %v)", f.Receipts, f.SpendMicros, err)
	}
	if _, err := Fold(append(evs, failEv(2, ""))); !errors.Is(err, domain.ErrTooManyReceipts) {
		t.Fatalf("receipt 4097 must still be refused: %v", err)
	}
}

func TestFoldSpendCapRefusesWithErrPolicy(t *testing.T) {
	mk := func(n int) []domain.Event {
		out := make([]domain.Event, n)
		for i := range out {
			out[i] = ev(domain.LadderRecord{Class: "NOT_COUNTED", Step: "spend", SpendMicros: 1, Ordinal: int64(i + 1)})
		}
		return out
	}
	if f, err := Fold(mk(domain.MaxLadderSpends)); err != nil || f.SpendMicros != domain.MaxLadderSpends {
		t.Fatalf("exactly the spend cap: (%d, %v)", f.SpendMicros, err)
	}
	_, err := Fold(mk(domain.MaxLadderSpends + 1))
	if !errors.Is(err, ErrPolicy) || !errors.Is(err, domain.ErrTooManyReceipts) || !strings.Contains(err.Error(), "4096") {
		t.Fatalf("one spend past the cap: %v, want ErrPolicy and ErrTooManyReceipts naming 4096", err)
	}
}

type oracleStep struct {
	Record domain.LadderRecord `json:"record"`
	Status struct {
		Rung           string   `json:"rung"`
		FailuresAtRung int      `json:"failures_at_rung"`
		Fingerprint    *string  `json:"fingerprint"`
		Climbs         int      `json:"climbs"`
		BlockedBy      []string `json:"blocked_by"`
		NeedsUser      bool     `json:"needs_user_permission"`
		Launches       int      `json:"launches"`
		SpendMicros    int64    `json:"spend_micros"`
	} `json:"status"`
}

type oracleFile struct {
	Reference struct {
		EscalationPy string `json:"escalation_py_sha256"`
		ModelTiers   string `json:"model_tiers_json_sha256"`
	} `json:"reference"`
	Seed     int64           `json:"seed"`
	Policy   json.RawMessage `json:"policy"`
	Steps    []oracleStep    `json:"steps"`
	Refusals []struct {
		Why    string              `json:"why"`
		Record domain.LadderRecord `json:"record"`
	} `json:"refusals"`
}

func loadOracle(t *testing.T) (oracleFile, Policy) {
	t.Helper()
	b, err := os.ReadFile("testdata/rung_oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	var o oracleFile
	if err := json.Unmarshal(b, &o); err != nil {
		t.Fatal(err)
	}
	p, err := DecodePolicy("oracle-policy.json", o.Policy)
	if err != nil {
		t.Fatalf("oracle policy: %v", err)
	}
	return o, p
}

// AC-1 (PL-9): at EVERY prefix of the escalation.py sequence, the Fabric fold
// equals ladder_status from the real Python ledger: rung, failures, fingerprint,
// climbs, blocked_by, launches and spend. "Live" is the incremental fold.
func TestOracleEveryPrefixEqualsEscalationPy(t *testing.T) {
	o, p := loadOracle(t)
	t.Logf("seed=%d steps=%d", o.Seed, len(o.Steps))
	if len(o.Steps) < 200 {
		t.Fatalf("seed %d: only %d oracle steps; the file is truncated or empty", o.Seed, len(o.Steps))
	}
	var log []domain.Event
	climbed, blocked, perm, fails, spends := false, false, false, 0, 0
	for i, s := range o.Steps {
		r := s.Record
		if err := domain.ValidateLadderRecord(&r); err != nil {
			t.Fatalf("seed %d step %d: escalation.py record refused by Fabric: %v", o.Seed, i, err)
		}
		log = append(log, domain.Event{Kind: domain.LadderEventKind(&r), Mission: "m_ladder-oracle", Seq: int64(i + 1), Ladder: &r})
		got, err := StatusOf(p, "oracle", log)
		if err != nil {
			t.Fatalf("seed %d step %d: %v", o.Seed, i, err)
		}
		w := s.Status
		eq := got.Rung == w.Rung && got.FailuresAtRung == w.FailuresAtRung && got.Climbs == w.Climbs &&
			((got.Fingerprint == nil) == (w.Fingerprint == nil)) && (got.Fingerprint == nil || *got.Fingerprint == *w.Fingerprint) &&
			strings.Join(got.BlockedBy, ",") == strings.Join(w.BlockedBy, ",") && got.NeedsUserPermission == w.NeedsUser &&
			got.Launches == w.Launches && got.SpendMicros == w.SpendMicros
		if !eq {
			t.Fatalf("seed %d step %d (%s %s): fabric %+v\n python %+v", o.Seed, i, r.Step, r.Class, got.LadderStatus, w)
		}
		climbed = climbed || w.Climbs > 0
		blocked = blocked || len(w.BlockedBy) > 0
		perm = perm || w.NeedsUser
		if r.Class == "FAIL" {
			fails++
		}
		if r.Step == "spend" {
			spends++
		}
	}
	if !climbed || !blocked || !perm || fails < 30 || spends < 3 {
		t.Fatalf("seed %d: the oracle sequence is vacuous (climbed=%v blocked=%v permission=%v fails=%d spends=%d)", o.Seed, climbed, blocked, perm, fails, spends)
	}
}

// CI-4: the oracle names the reference implementation it came from. Where that
// implementation is present (LADDER_REFERENCE_HOOKS, else ~/.claude/hooks), its
// current sha256 must match, so a changed escalation.py or model_tiers.json is
// noticed instead of the oracle quietly going stale.
func TestOracleReferenceMatchesTheHooks(t *testing.T) {
	o, _ := loadOracle(t)
	hexRe := regexp.MustCompile(`^[0-9a-f]{64}$`)
	if !hexRe.MatchString(o.Reference.EscalationPy) || !hexRe.MatchString(o.Reference.ModelTiers) {
		t.Fatalf("the oracle carries no reference sha256s: %+v", o.Reference)
	}
	dir := os.Getenv("LADDER_REFERENCE_HOOKS")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".claude", "hooks")
	}
	for name, want := range map[string]string{"escalation.py": o.Reference.EscalationPy, "model_tiers.json": o.Reference.ModelTiers} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Skipf("reference %s not present (%v); set LADDER_REFERENCE_HOOKS to compare", name, err)
		}
		sum := sha256.Sum256(b)
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Fatalf("%s is %s but the oracle was generated from %s: regenerate it with tools/ladder-parity/gen_events.py and review the diff", name, got[:12], want[:12])
		}
	}
}

// NEG-4 / AC-3: records escalation.py would store but Fabric must refuse are a
// reported refusal, never silently stored.
func TestOracleRefusals(t *testing.T) {
	o, _ := loadOracle(t)
	if len(o.Refusals) == 0 {
		t.Fatal("no refusal cases in the oracle")
	}
	for _, c := range o.Refusals {
		r := c.Record
		if err := domain.ValidateLadderRecord(&r); !errors.Is(err, domain.ErrInvalidReceipt) {
			t.Errorf("%s: %v, want ErrInvalidReceipt", c.Why, err)
		}
	}
}
