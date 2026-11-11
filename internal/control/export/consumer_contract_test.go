package export

import (
	"encoding/json"
	"math"
	"os"
	"reflect"
	"regexp"
	"testing"
)

func TestMissionControlProducerFixtures(t *testing.T) {
	var fixture struct {
		Version int                       `json:"version"`
		Max     int                       `json:"cursor_max_bytes"`
		Cursors map[string]string         `json:"cursors"`
		Events  map[string]map[string]any `json:"events"`
	}
	raw, err := os.ReadFile("testdata/mc-consumer-contract.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Version != 1 || fixture.Max != 128 {
		t.Fatal("consumer fixture contract changed")
	}
	for name, cursor := range fixture.Cursors {
		if len(cursor) > 128 || !regexp.MustCompile(`^[A-Za-z0-9._:=-]+$`).MatchString(cursor) {
			t.Errorf("%s cannot pass the transport: %q", name, cursor)
		}
		if _, _, _, _, err := decodeCursor(cursor); err != nil {
			t.Errorf("%s is not a valid producer cursor: %v", name, err)
		}
	}
	lag := MapLag(1, 2, at)
	lag.Historic = true
	want := map[string]Event{"lag": lag}
	for i, item := range []struct{ name, kind string }{{"old1", "MISSION_ADMITTED"}, {"old2", "EVIDENCE_RECORDED"}, {"new3", "DELIVERED"}} {
		ev := MapRow(Row{ID: int64(i + 1), Mission: "m_fixture", Seq: int64(i + 1), Kind: item.kind, At: at, Ticket: "GoodQA goodqa#15361"})
		ev.Historic = i < 2
		want[item.name] = ev
	}
	if len(want) != len(fixture.Events) {
		t.Fatal("producer fixture inventory drifted")
	}
	for name, event := range want {
		encoded, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if err := json.Unmarshal(encoded, &body); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(body, fixture.Events[name]) {
			t.Errorf("%s fixture differs from actual producer: %s", name, encoded)
		}
		if bad := validate(loadSchema(t), body, "$"); len(bad) != 0 {
			t.Errorf("%s does not match the shared schema: %v", name, bad)
		}
	}
	// Largest representable outbox position stays inside the actual adapter's
	// safe opaque-cursor contract, for both historic and live mode.
	for _, historic := range []bool{false, true} {
		cursor := encodeCursor("0123456789abcdef", math.MaxInt64, "abcdef123456", historic)
		if len(cursor) > 128 || !regexp.MustCompile(`^[A-Za-z0-9._:=-]+$`).MatchString(cursor) {
			t.Fatalf("producer cursor exceeds the transport: %q", cursor)
		}
	}
}
