package export

import (
	"crypto/sha1" //nolint:gosec // git blob ids are SHA-1; this pins a file, it is not a security check
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"fabric/internal/domain"
)

const pinnedBlob = "d7506f0d224dd83d9105ef2f8f443f5fe5efb86a" // agent-harness d825d66 docs/schemas/mc-event-v1.schema.json

func loadSchema(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("testdata/mc-event-v1.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	h := sha1.New() //nolint:gosec
	fmt.Fprintf(h, "blob %d\x00", len(raw))
	h.Write(raw)
	if got := hex.EncodeToString(h.Sum(nil)); got != pinnedBlob {
		t.Fatalf("testdata schema blob %s, pinned %s: the copy drifted from agent-harness d825d66", got, pinnedBlob)
	}
	var s map[string]any
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// validate checks a JSON document against the keywords the pinned schema
// uses: type, const, enum, pattern, maxLength, minimum, maximum, required and
// additionalProperties:false. It returns the violations.
func validate(schema map[string]any, v any, path string) []string {
	var bad []string
	if c, ok := schema["const"]; ok && v != c {
		bad = append(bad, fmt.Sprintf("%s: want const %v", path, c))
	}
	if e, ok := schema["enum"].([]any); ok {
		found := false
		for _, x := range e {
			if x == v {
				found = true
			}
		}
		if !found {
			bad = append(bad, fmt.Sprintf("%s: %v not in enum", path, v))
		}
	}
	switch schema["type"] {
	case "string":
		s, ok := v.(string)
		if !ok {
			return append(bad, path+": not a string")
		}
		if p, ok := schema["pattern"].(string); ok && !regexp.MustCompile(p).MatchString(s) {
			bad = append(bad, fmt.Sprintf("%s: %q does not match %s", path, s, p))
		}
		if m, ok := schema["maxLength"].(float64); ok && float64(utf8.RuneCountInString(s)) > m {
			bad = append(bad, fmt.Sprintf("%s: longer than %v", path, m))
		}
	case "integer":
		f, ok := v.(float64)
		if !ok || f != float64(int64(f)) {
			return append(bad, path+": not an integer")
		}
		if m, ok := schema["minimum"].(float64); ok && f < m {
			bad = append(bad, path+": below minimum")
		}
		if m, ok := schema["maximum"].(float64); ok && f > m {
			bad = append(bad, path+": above maximum")
		}
	case "object":
		o, ok := v.(map[string]any)
		if !ok {
			return append(bad, path+": not an object")
		}
		props, _ := schema["properties"].(map[string]any)
		if req, ok := schema["required"].([]any); ok {
			for _, r := range req {
				if _, ok := o[r.(string)]; !ok {
					bad = append(bad, fmt.Sprintf("%s: missing %v", path, r))
				}
			}
		}
		for k, val := range o {
			ps, known := props[k].(map[string]any)
			if !known {
				if ap, ok := schema["additionalProperties"].(bool); ok && !ap {
					bad = append(bad, fmt.Sprintf("%s.%s: unknown field", path, k))
				}
				continue
			}
			bad = append(bad, validate(ps, val, path+"."+k)...)
		}
	}
	return bad
}

var at = time.Date(2026, 11, 9, 12, 30, 45, 123_456_789, time.UTC)

func TestMapEvents(t *testing.T) {
	schema := loadSchema(t)
	long := "m_" + strings.Repeat("x", 150) // longer than the id shape allows

	rows := []Row{
		{ID: 1, Mission: "m_a", Seq: 1, Kind: "MISSION_ADMITTED", At: at},
		{ID: 2, Mission: "m_a", Seq: 2, Kind: "DELIVERED", At: at},
		{ID: 3, Mission: "m_a", Seq: 3, Kind: "LIVE_VERIFIED", At: at},
		{ID: 4, Mission: "m_a", Seq: 4, Kind: "ATTEMPT_UNKNOWN", At: at},
		{ID: 5, Mission: "m_b", Seq: 1, Kind: "FROM_THE_FUTURE", At: at},                // unknown kind
		{ID: 6, Mission: "m_c", Seq: 9, Kind: "MISSION_ADMITTED", At: at, Held: true},   // held stream
		{ID: 7, Mission: "m_d", Seq: 1, Kind: "", At: at},                               // event row missing
		{ID: 8, Mission: long, Seq: 1, Kind: "MISSION_ADMITTED", At: at},                // hostile id
		{ID: 9, Mission: "m_e\x1b[31m\n", Seq: 1, Kind: "x\"}{\"summary\":\"p", At: at}, // control chars and quotes in text fields
		{ID: 1 << 40, Mission: "m_z", Seq: 1 << 40, Kind: "MISSION_PARKED", At: at.In(time.FixedZone("x", 3600))},
	}
	for _, k := range domain.KnownEventKinds() {
		rows = append(rows, Row{ID: 100, Mission: "m_k", Seq: 1, Kind: k, At: at})
	}
	for _, r := range rows {
		ev := MapRow(r)
		line, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		var doc any
		if err := json.Unmarshal(line, &doc); err != nil {
			t.Fatal(err)
		}
		if bad := validate(schema, doc, "$"); len(bad) > 0 {
			t.Errorf("row %+v: %s\nline: %s", r, strings.Join(bad, "; "), line)
		}
		if !strings.HasPrefix(ev.ID, "fabric:") {
			t.Errorf("id %q lacks the fabric: prefix", ev.ID)
		}
		if utf8.RuneCountInString(ev.Summary) > MaxSummary {
			t.Errorf("summary %d runes", utf8.RuneCountInString(ev.Summary))
		}
		for _, c := range ev.Summary {
			if c < 0x20 || c == 0x7f {
				t.Errorf("control character in summary %q", ev.Summary)
			}
		}
	}

	ev := MapRow(rows[0])
	if ev.ID != "fabric:1" || ev.TS != "2026-11-09T12:30:45.123Z" || ev.Subject.Initiative != "m_a" || ev.Kind != "owner" {
		t.Errorf("basic mapping: %+v", ev)
	}
	if ev := MapRow(rows[9]); ev.TS != "2026-11-09T12:30:45.123Z" || ev.ID != "fabric:1099511627776" {
		t.Errorf("ts must be UTC event time and the id the outbox id: %+v", ev)
	}
	if ev := MapRow(rows[1]); ev.Kind != "release" {
		t.Errorf("DELIVERED: %+v", ev)
	}
	// NEG-1: held and unknown are system with a fixed summary.
	for _, i := range []int{4, 5, 6} {
		ev := MapRow(rows[i])
		if ev.Kind != "system" {
			t.Errorf("row %d must be kind system: %+v", rows[i].ID, ev)
		}
	}
	if s := MapRow(rows[5]).Summary; s != "mission m_c: stream held, event withheld (seq 9)" {
		t.Errorf("held summary %q", s)
	}
	if s := MapRow(rows[8]); strings.Contains(s.Summary, "\x1b") || s.Subject.Initiative != "invalid" || strings.Contains(s.Summary, "summary") {
		t.Errorf("hostile text must not be echoed: %+v", s)
	}
	if s := MapRow(rows[7]); s.Subject.Initiative != "invalid" {
		t.Errorf("over-long mission id echoed: %+v", s)
	}
}

// Payload text never reaches a line: MapRow has no payload input, and the
// golden below pins the exact bytes of one line.
func TestMapGolden(t *testing.T) {
	line, err := json.Marshal(MapRow(Row{ID: 42, Mission: "m_gold", Seq: 7, Kind: "ATTEMPT_UNKNOWN", At: at}))
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"schema":"mc.event/1","id":"fabric:42","ts":"2026-11-09T12:30:45.123Z","kind":"owner","source":"fabric","subject":{"initiative":"m_gold"},"summary":"mission m_gold: attempt unknown (seq 7)","severity":"warn"}`
	if string(line) != want {
		t.Fatalf("got  %s\nwant %s", line, want)
	}
}
