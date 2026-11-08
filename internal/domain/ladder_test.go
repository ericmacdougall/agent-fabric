package domain

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// goodqa#15324 S1b: ladder receipts are events on a per-task ladder mission.
// Python (escalation.py) classifies; Fabric validates the shape and stores.

func TestLadderMissionID(t *testing.T) {
	ok55 := strings.Repeat("a", 55)
	for _, task := range []string{"goodqa-15299", "T1", "a.b_c-d", ok55} {
		id, err := LadderMissionID(task)
		if err != nil {
			t.Fatalf("task %q refused: %v", task, err)
		}
		if string(id) != LadderMissionPrefix+task {
			t.Fatalf("task %q gave %q", task, id)
		}
		if _, err := ParseMissionID(string(id)); err != nil {
			t.Fatalf("ladder id %q does not parse as a mission id: %v", id, err)
		}
		if !IsLadderMission(id) {
			t.Fatalf("%q not recognised as a ladder mission", id)
		}
	}
	bad := map[string]string{
		"empty": "", "56 bytes": strings.Repeat("a", 56), "64 bytes": strings.Repeat("a", 64),
		"leading dot": ".x", "leading dash": "-x", "space": "a b", "slash": "a/b", "colon": "a:b",
		"newline": "a\nb", "unicode": "tést", "fullwidth": "ｔask", "nul": "a\x00b",
	}
	for name, task := range bad {
		if id, err := LadderMissionID(task); !errors.Is(err, ErrInvalidID) || id != "" {
			t.Errorf("%s: task %q gave (%q, %v), want ErrInvalidID and no id", name, task, id, err)
		}
	}
}

func TestIsLadderMissionIsPrefixOnly(t *testing.T) {
	for _, id := range []MissionID{"m_a", "m_ladder", "m_ladderx", "M_ladder-x", "ladder-x", " m_ladder-x", "m_Ladder-x", ""} {
		if IsLadderMission(id) {
			t.Errorf("%q taken as a ladder mission", id)
		}
	}
	if !IsLadderMission("m_ladder-x") {
		t.Error("m_ladder-x not a ladder mission")
	}
}

func s(v string) *string { return &v }

var (
	fp64 = strings.Repeat("ab", 32)
	sha  = strings.Repeat("cd", 32)
	h40  = strings.Repeat("ef", 20)
)

func review(class string) LadderRecord {
	r := LadderRecord{Class: class, Step: "review", Rsha: s(sha), Src: "review", Launched: true}
	if class == "FAIL" {
		r.Fp = s(fp64)
	}
	return r
}

// The shape rules. Each case names the escalation.py behaviour it follows.
func TestValidateLadderRecord(t *testing.T) {
	mut := func(base LadderRecord, f func(*LadderRecord)) LadderRecord { f(&base); return base }
	good := map[string]LadderRecord{
		"review FAIL":               review("FAIL"),
		"review PASS":               review("PASS"),
		"review NOT_COUNTED":        mut(review("NOT_COUNTED"), func(r *LadderRecord) { r.Launched = false }),
		"review counted with spend": mut(review("FAIL"), func(r *LadderRecord) { r.Counted, r.SpendMicros = true, 12_500_000 }),
		"test FAIL":                 mut(review("FAIL"), func(r *LadderRecord) { r.Step = "test" }),
		"spec_review FAIL w/ ids":   mut(review("FAIL"), func(r *LadderRecord) { r.Step = "spec_review"; r.IDs = []string{"RT-1", "F2"} }),
		"head on a receipt":         mut(review("FAIL"), func(r *LadderRecord) { r.Commit = s(h40) }),
		"short head 7 hex":          mut(review("FAIL"), func(r *LadderRecord) { r.Commit = s("abcdef0") }),
		"claim":                     {Class: "CLAIM", Step: "claim", Commit: s(h40), Src: "claim-fix"},
		"spend":                     {Class: "NOT_COUNTED", Step: "spend", SpendMicros: 500_000, Ordinal: 1, Src: "add-spend"},
		"approve_top":               {Step: "approve_top", Approval: &LadderApproval{Ask: "a_1", Src: "user:s/u \"ok\"", By: "op:eric"}},
		"approve answer: source":    {Step: "approve_top", Approval: &LadderApproval{Ask: "a_1", Src: "answer:x", By: "op:eric"}},
	}
	for name, r := range good {
		r := r
		if err := ValidateLadderRecord(&r); err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}
	bad := map[string]LadderRecord{
		"unknown class":                 mut(review("FAIL"), func(r *LadderRecord) { r.Class = "MAYBE" }),
		"empty class on a review":       mut(review("PASS"), func(r *LadderRecord) { r.Class = "" }),
		"lowercase class":               mut(review("PASS"), func(r *LadderRecord) { r.Class = "pass" }),
		"unknown step":                  mut(review("PASS"), func(r *LadderRecord) { r.Step = "other" }),
		"CLAIM class on a review":       mut(review("PASS"), func(r *LadderRecord) { r.Class = "CLAIM" }),
		"FAIL without fingerprint":      mut(review("FAIL"), func(r *LadderRecord) { r.Fp = nil }),
		"PASS with fingerprint":         mut(review("PASS"), func(r *LadderRecord) { r.Fp = s(fp64) }),
		"fingerprint short":             mut(review("FAIL"), func(r *LadderRecord) { r.Fp = s(fp64[:63]) }),
		"fingerprint uppercase":         mut(review("FAIL"), func(r *LadderRecord) { r.Fp = s(strings.ToUpper(fp64)) }),
		"fingerprint is a finding id":   mut(review("FAIL"), func(r *LadderRecord) { r.Fp = s("RT-1") }),
		"head 6 hex":                    mut(review("FAIL"), func(r *LadderRecord) { r.Commit = s("abcdef") }),
		"head 65 hex":                   mut(review("FAIL"), func(r *LadderRecord) { r.Commit = s(strings.Repeat("a", 65)) }),
		"head uppercase":                mut(review("FAIL"), func(r *LadderRecord) { r.Commit = s("ABCDEF0") }),
		"head not hex":                  mut(review("FAIL"), func(r *LadderRecord) { r.Commit = s("not-a-sha") }),
		"claim without head":            {Class: "CLAIM", Step: "claim"},
		"claim with a non-hex head":     {Class: "CLAIM", Step: "claim", Commit: s("HEAD")},
		"claim with a receipt sha":      {Class: "CLAIM", Step: "claim", Commit: s(h40), Rsha: s(sha)},
		"review without receipt sha":    mut(review("PASS"), func(r *LadderRecord) { r.Rsha = nil }),
		"receipt sha short":             mut(review("PASS"), func(r *LadderRecord) { r.Rsha = s(sha[:63]) }),
		"FAIL review not launched":      mut(review("FAIL"), func(r *LadderRecord) { r.Launched = false }),
		"counted but not launched":      mut(review("NOT_COUNTED"), func(r *LadderRecord) { r.Launched, r.Counted = false, true }),
		"counted PASS":                  mut(review("PASS"), func(r *LadderRecord) { r.Counted = true }),
		"counted test step":             mut(review("FAIL"), func(r *LadderRecord) { r.Step, r.Counted = "test", true }),
		"negative spend":                mut(review("FAIL"), func(r *LadderRecord) { r.SpendMicros = -1 }),
		"huge spend":                    mut(review("FAIL"), func(r *LadderRecord) { r.SpendMicros = 1_000_000_000_001 }),
		"spend on an uncounted receipt": mut(review("PASS"), func(r *LadderRecord) { r.SpendMicros = 5 }),
		"spend step without ordinal":    {Class: "NOT_COUNTED", Step: "spend", SpendMicros: 5},
		"spend step zero":               {Class: "NOT_COUNTED", Step: "spend", Ordinal: 1},
		"spend step with receipt sha":   {Class: "NOT_COUNTED", Step: "spend", SpendMicros: 5, Ordinal: 1, Rsha: s(sha)},
		"ordinal on a review":           mut(review("PASS"), func(r *LadderRecord) { r.Ordinal = 3 }),
		"ordinal too large":             {Class: "NOT_COUNTED", Step: "spend", SpendMicros: 5, Ordinal: 1_000_000_001},
		"id starts with digit":          mut(review("FAIL"), func(r *LadderRecord) { r.IDs = []string{"1x"} }),
		"id with newline":               mut(review("FAIL"), func(r *LadderRecord) { r.IDs = []string{"a\nb"} }),
		"id 65":                         mut(review("FAIL"), func(r *LadderRecord) { r.IDs = []string{"A" + strings.Repeat("b", 64)} }),
		"id UNSPECIFIED":                mut(review("FAIL"), func(r *LadderRecord) { r.IDs = []string{"unspecified"} }),
		"33 ids": mut(review("FAIL"), func(r *LadderRecord) {
			r.IDs = make([]string, 33)
			for i := range r.IDs {
				r.IDs[i] = "F"
			}
		}),
		"src with a space":            mut(review("PASS"), func(r *LadderRecord) { r.Src = "a b" }),
		"src 65":                      mut(review("PASS"), func(r *LadderRecord) { r.Src = strings.Repeat("a", 65) }),
		"approval on a review":        mut(review("PASS"), func(r *LadderRecord) { r.Approval = &LadderApproval{Ask: "a_1", Src: "user:x", By: "op:e"} }),
		"approve without approval":    {Step: "approve_top"},
		"approve with a class":        {Class: "PASS", Step: "approve_top", Approval: &LadderApproval{Ask: "a_1", Src: "user:x", By: "op:e"}},
		"approve inferred source":     {Step: "approve_top", Approval: &LadderApproval{Ask: "a_1", Src: "inferred: from x", By: "op:e"}},
		"approve ticket source":       {Step: "approve_top", Approval: &LadderApproval{Ask: "a_1", Src: "ticket:GoodQA goodqa#1", By: "op:e"}},
		"approve wal source":          {Step: "approve_top", Approval: &LadderApproval{Ask: "a_1", Src: "wal:/x#w1", By: "op:e"}},
		"approve file source":         {Step: "approve_top", Approval: &LadderApproval{Ask: "a_1", Src: "file:/x:1@abc", By: "op:e"}},
		"approve pasted source":       {Step: "approve_top", Approval: &LadderApproval{Ask: "a_1", Src: "pasted:s/u \"x\"", By: "op:e"}},
		"approve uuid source":         {Step: "approve_top", Approval: &LadderApproval{Ask: "a_1", Src: "uuid:abc", By: "op:e"}},
		"approve bare user:":          {Step: "approve_top", Approval: &LadderApproval{Ask: "a_1", Src: "user:", By: "op:e"}},
		"approve source with newline": {Step: "approve_top", Approval: &LadderApproval{Ask: "a_1", Src: "user:x\ninferred: y", By: "op:e"}},
		"approve empty source":        {Step: "approve_top", Approval: &LadderApproval{Ask: "a_1", By: "op:e"}},
		"approve agent source":        {Step: "approve_top", Approval: &LadderApproval{Ask: "a_1", Src: "agent:x", By: "op:e"}},
		"approve by non operator":     {Step: "approve_top", Approval: &LadderApproval{Ask: "a_1", Src: "user:x", By: "chat"}},
		"approve by bare op:":         {Step: "approve_top", Approval: &LadderApproval{Ask: "a_1", Src: "user:x", By: "op:"}},
		"approve without ask":         {Step: "approve_top", Approval: &LadderApproval{Src: "user:x", By: "op:e"}},
		"approve with a receipt sha":  {Step: "approve_top", Rsha: s(sha), Approval: &LadderApproval{Ask: "a_1", Src: "user:x", By: "op:e"}},
	}
	for name, r := range bad {
		r := r
		if err := ValidateLadderRecord(&r); !errors.Is(err, ErrInvalidReceipt) {
			t.Errorf("%s: got %v, want ErrInvalidReceipt", name, err)
		}
	}
	if err := ValidateLadderRecord(nil); !errors.Is(err, ErrInvalidReceipt) {
		t.Errorf("nil record: %v", err)
	}
}

// 9abc522 Event and Command JSON keys, pinned from internal/domain/events.go
// at 9abc522. PL-2: Go matches JSON keys case-insensitively, and a mistyped
// colliding key in an outbox payload makes OutboxPage fail and fabd exit.
var oldEventKeys = []string{"kind", "mission", "seq", "attempt", "fence", "revision", "generation", "head", "host", "role",
	"path", "lock_path", "worktree", "receipt", "outcome", "prelaunch", "count", "backoff_ms", "wait", "wait_key", "token",
	"effect", "result", "submission", "reason", "attempt_to", "effect_kind", "request_sha", "occurrence", "vacuous", "claims", "target"}
var oldCommandKeys = []string{"kind", "mission", "attempt", "fence", "revision", "generation", "head", "host", "role", "path",
	"lock_path", "worktree", "receipt", "outcome", "prelaunch", "capacity_wait", "wait_key", "token", "effect", "result",
	"submission", "reason", "target", "effect_kind", "request", "vacuous", "claims"}

func jsonKeys(t reflect.Type) []string {
	var out []string
	for i := 0; i < t.NumField(); i++ {
		tag := strings.Split(t.Field(i).Tag.Get("json"), ",")[0]
		if tag != "" && tag != "-" {
			out = append(out, tag)
		}
	}
	return out
}

func TestLadderKeysNeverCollideWithOldEventOrCommandKeys(t *testing.T) {
	old := map[string]bool{}
	for _, k := range append(append([]string{}, oldEventKeys...), oldCommandKeys...) {
		old[strings.ToLower(k)] = true
	}
	// Every OLD key must still be present, unchanged, on the current structs.
	have := map[string]bool{}
	for _, k := range jsonKeys(reflect.TypeOf(Event{})) {
		have[k] = true
	}
	for _, k := range oldEventKeys {
		if !have[k] {
			t.Errorf("old event key %q is gone", k)
		}
	}
	// The only new top-level key on Event and Command is `ladder`.
	for _, ty := range []reflect.Type{reflect.TypeOf(Event{}), reflect.TypeOf(Command{})} {
		added := 0
		for _, k := range jsonKeys(ty) {
			if !old[strings.ToLower(k)] {
				added++
				if k != "ladder" {
					t.Errorf("%s gained top-level key %q; ladder data must nest under `ladder`", ty.Name(), k)
				}
			}
		}
		if added != 1 {
			t.Errorf("%s gained %d new top-level keys, want exactly 1", ty.Name(), added)
		}
	}
	for _, k := range append(jsonKeys(reflect.TypeOf(LadderRecord{})), jsonKeys(reflect.TypeOf(LadderApproval{}))...) {
		if old[strings.ToLower(k)] || strings.EqualFold(k, "ladder") {
			t.Errorf("ladder field %q collides (ignoring case) with an old Event/Command key", k)
		}
	}
	if old["ladder"] {
		t.Error("`ladder` was already an Event/Command key")
	}
}

// The 9abc522 Event, as the old OutboxPage decodes it. A ladder payload must
// decode without error and set nothing but kind, mission and seq.
type oldEvent struct {
	Kind       string `json:"kind"`
	Mission    string `json:"mission"`
	Seq        int64  `json:"seq,omitempty"`
	Attempt    string `json:"attempt,omitempty"`
	Fence      int64  `json:"fence,omitempty"`
	Revision   int64  `json:"revision,omitempty"`
	Generation int64  `json:"generation,omitempty"`
	Head       string `json:"head,omitempty"`
	Host       string `json:"host,omitempty"`
	Role       string `json:"role,omitempty"`
	Path       string `json:"path,omitempty"`
	LockPath   string `json:"lock_path,omitempty"`
	Worktree   string `json:"worktree,omitempty"`
	Receipt    string `json:"receipt,omitempty"`
	Outcome    string `json:"outcome,omitempty"`
	Prelaunch  bool   `json:"prelaunch,omitempty"`
	Count      int    `json:"count,omitempty"`
	BackoffMS  int64  `json:"backoff_ms,omitempty"`
	Wait       bool   `json:"wait,omitempty"`
	WaitKey    string `json:"wait_key,omitempty"`
	Token      string `json:"token,omitempty"`
	Effect     string `json:"effect,omitempty"`
	Result     string `json:"result,omitempty"`
	Submission int    `json:"submission,omitempty"`
	Reason     string `json:"reason,omitempty"`
	AttemptTo  string `json:"attempt_to,omitempty"`
	EffectKind string `json:"effect_kind,omitempty"`
	RequestSHA string `json:"request_sha,omitempty"`
	Occurrence int    `json:"occurrence,omitempty"`
	Vacuous    bool   `json:"vacuous,omitempty"`
	Claims     []struct {
		AC      string `json:"ac"`
		Receipt string `json:"receipt"`
	} `json:"claims,omitempty"`
	Target string `json:"target,omitempty"`
}

func TestLadderPayloadDecodesIntoTheOldEvent(t *testing.T) {
	recs := []LadderRecord{
		{Class: "FAIL", Step: "review", Fp: s(fp64), Commit: s(h40), Rsha: s(sha), Src: "review", Launched: true, Counted: true,
			SpendMicros: 12_500_000, IDs: []string{"RT-1"}},
		{Class: "NOT_COUNTED", Step: "spend", SpendMicros: 5, Ordinal: 7, Src: "add-spend"},
		{Step: "approve_top", Approval: &LadderApproval{Ask: "a_1", Src: "user:s/u", By: "op:eric"}},
	}
	for i, r := range recs {
		r := r
		for _, kind := range []EventKind{EvLadderReceipt, EvLadderTopApproved} {
			b, err := json.Marshal(Event{Kind: kind, Mission: "m_ladder-t", Seq: 3, Ladder: &r})
			if err != nil {
				t.Fatal(err)
			}
			var o oldEvent
			if err := json.Unmarshal(b, &o); err != nil {
				t.Fatalf("record %d %s: the 9abc522 Event cannot decode %s: %v", i, kind, b, err)
			}
			if o.Kind != string(kind) || o.Mission != "m_ladder-t" || o.Seq != 3 {
				t.Fatalf("record %d: kind/mission/seq lost: %+v", i, o)
			}
			o.Kind, o.Mission, o.Seq = "", "", 0
			if !reflect.DeepEqual(o, oldEvent{}) {
				t.Fatalf("record %d %s: ladder payload set old Event fields: %+v", i, kind, o)
			}
		}
	}
	// The failing control for the check above: a colliding key does break the
	// old decode, so the test can fail.
	var o oldEvent
	if err := json.Unmarshal([]byte(`{"kind":"LADDER_RECEIPT","mission":"m","COUNT":"x"}`), &o); err == nil {
		t.Fatal("control: a mistyped colliding key decoded without error; the collision test proves nothing")
	}
}

// Existing events and commands stay byte-identical: no `ladder` key unless set.
func TestOldEventsAndCommandsHaveNoLadderKey(t *testing.T) {
	e, _ := json.Marshal(Event{Kind: EvMissionAdmitted, Mission: "m_a", Seq: 1, Count: 2, Head: h40})
	c, _ := json.Marshal(Command{Kind: CmdAdmitMission, Mission: "m_a", Head: h40})
	if strings.Contains(string(e), "ladder") || strings.Contains(string(c), "ladder") {
		t.Fatalf("a ladder key leaked into old shapes: %s %s", e, c)
	}
}

func TestLadderKindsAreKnown(t *testing.T) {
	for _, k := range []EventKind{EvLadderReceipt, EvLadderTopApproved} {
		if !KnownEventKind(k) {
			t.Errorf("%s is not a known event kind", k)
		}
	}
	if string(EvLadderReceipt) != "LADDER_RECEIPT" || string(EvLadderTopApproved) != "LADDER_TOP_APPROVED" || string(CmdRecordLadderReceipt) != "RECORD_LADDER_RECEIPT" {
		t.Fatalf("kind names drifted: %q %q %q", EvLadderReceipt, EvLadderTopApproved, CmdRecordLadderReceipt)
	}
}
