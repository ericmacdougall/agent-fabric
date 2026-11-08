package ladder

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"
)

type parityFile struct {
	Seed      int64                      `json:"seed"`
	Reference map[string]string          `json:"reference_sha256"`
	Policies  map[string]json.RawMessage `json:"policies"`
	Vectors   []struct {
		Name   string          `json:"name"`
		Policy string          `json:"policy"`
		State  json.RawMessage `json:"state"`
		Want   LadderStatus    `json:"want"`
	} `json:"vectors"`
}

func decodeStrict(t *testing.T, raw []byte, v any) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

// TestReplayParityWithEscalationPy checks every vector that
// tools/ladder-parity/gen.py produced by running escalation.py's own
// ladder_status. Regenerate with:
//
//	python3 -I tools/ladder-parity/gen.py ~/.claude/hooks internal/ladder/testdata/parity.json 15324
func TestReplayParityWithEscalationPy(t *testing.T) {
	raw, err := os.ReadFile("testdata/parity.json")
	if err != nil {
		t.Fatal(err)
	}
	var pf parityFile
	decodeStrict(t, raw, &pf)
	if len(pf.Vectors) < 50 {
		t.Fatalf("seed %d: only %d parity vectors; the file is truncated or empty", pf.Seed, len(pf.Vectors))
	}
	policies := map[string]Policy{}
	for name, body := range pf.Policies {
		var p Policy
		decodeStrict(t, body, &p)
		policies[name] = p
	}
	for _, v := range pf.Vectors {
		p, ok := policies[v.Policy]
		if !ok {
			t.Fatalf("seed %d %s: unknown policy %q", pf.Seed, v.Name, v.Policy)
		}
		var st LedgerState
		decodeStrict(t, v.State, &st)
		got, err := Status(p, st)
		if err != nil {
			t.Fatalf("seed %d %s: %v", pf.Seed, v.Name, err)
		}
		if !reflect.DeepEqual(got, v.Want) {
			gj, _ := json.Marshal(got)
			wj, _ := json.Marshal(v.Want)
			t.Errorf("seed %d %s:\n got  %s\n want %s", pf.Seed, v.Name, gj, wj)
		}
	}
}

func testPolicy() Policy {
	return Policy{
		LowestRung: "R0", FailuresPerRung: 2,
		Rungs: map[string]PolicyRung{
			"R0": {Writable: true}, "R1": {Writable: true}, "R2": {Writable: false},
			"R3": {Writable: true, NeedsUserPermission: true},
		},
		Limits: Limits{MaxReviewLaunches: 8, MaxRungClimbs: 3, MaxTotalReviewUSD: "60"},
	}
}

func s(v string) *string { return &v }

func TestReplaySkipsNonWritableRung(t *testing.T) {
	ev := []LedgerEvent{
		{Class: "FAIL", Fingerprint: s("a"), Head: s("h1")}, {Class: "FAIL", Fingerprint: s("a"), Head: s("h2")},
		{Class: "FAIL", Fingerprint: s("b"), Head: s("h3")}, {Class: "FAIL", Fingerprint: s("b"), Head: s("h4")},
	}
	got, err := Replay(testPolicy(), ev)
	if err != nil {
		t.Fatal(err)
	}
	if got.Rung != "R3" || got.Climbs != 2 || got.Fingerprint != nil {
		t.Fatalf("got %+v, want R3 after 2 climbs skipping non-writable R2", got)
	}
}

func TestPolicyValidateRefuses(t *testing.T) {
	cases := map[string]func(*Policy){
		"zero failures":  func(p *Policy) { p.FailuresPerRung = 0 },
		"unknown lowest": func(p *Policy) { p.LowestRung = "R9" },
		"bad rung name":  func(p *Policy) { p.Rungs["X1"] = PolicyRung{} },
		"bad usd":        func(p *Policy) { p.Limits.MaxTotalReviewUSD = "sixty" },
	}
	for name, mut := range cases {
		p := testPolicy()
		p.Rungs = map[string]PolicyRung{}
		for k, v := range testPolicy().Rungs {
			p.Rungs[k] = v
		}
		mut(&p)
		if _, err := Replay(p, nil); !errors.Is(err, ErrPolicy) {
			t.Errorf("%s: err = %v, want ErrPolicy", name, err)
		}
	}
}

func TestStatusSpendComparesLikePythonFloat(t *testing.T) {
	p := testPolicy()
	p.Limits.MaxTotalReviewUSD = "0.3"
	for spend, blocked := range map[string]bool{"0.29999999999999999": true, "0.2999999999999999": false, "0.3": true, "0.30000000000000004": true} {
		st, err := Status(p, LedgerState{SpendUSD: json.Number(spend)})
		if err != nil {
			t.Fatal(err)
		}
		has := len(st.BlockedBy) == 1 && st.BlockedBy[0] == "max_total_review_usd"
		if has != blocked {
			t.Errorf("spend %s: blocked_by %v, want blocked=%v", spend, st.BlockedBy, blocked)
		}
	}
	if _, err := Status(p, LedgerState{SpendUSD: "1e"}); err == nil {
		t.Error("malformed spend_usd accepted")
	}
}
