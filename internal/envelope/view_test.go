package envelope

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

type vectorFile struct {
	Cases []struct {
		Name      string `json:"name"`
		Mission   string `json:"mission"`
		Seq       int64  `json:"seq"`
		Kind      string `json:"kind"`
		Actor     string `json:"actor"`
		AtMicros  int64  `json:"at_micros"`
		Payload   string `json:"payload"`
		EventID   string `json:"event_id"`
		Canonical string `json:"canonical"`
	} `json:"cases"`
	Refused []struct {
		Name     string `json:"name"`
		Mission  string `json:"mission"`
		Seq      int64  `json:"seq"`
		AtMicros int64  `json:"at_micros"`
	} `json:"refused"`
}

func loadVectors(t *testing.T) vectorFile {
	t.Helper()
	b, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectorFile
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Cases) < 5 {
		t.Fatalf("vector file holds %d cases", len(v.Cases))
	}
	return v
}

// The vectors come from tools/gen_vectors.py, which shares no code with this
// package: the event id and the canonical bytes must match it exactly.
func TestEventIDVectors(t *testing.T) {
	for _, c := range loadVectors(t).Cases {
		at := time.UnixMicro(c.AtMicros)
		got, err := EventID(c.Mission, c.Seq, at)
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		if got != c.EventID {
			t.Errorf("%s: event id %s, vector %s", c.Name, got, c.EventID)
		}
		if !ValidEventID(got) {
			t.Errorf("%s: %s does not have the event id shape", c.Name, got)
		}
	}
	for _, r := range loadVectors(t).Refused {
		if _, err := EventID(r.Mission, r.Seq, time.UnixMicro(r.AtMicros)); err == nil {
			t.Errorf("%s: a mission %q was accepted", r.Name, r.Mission)
		}
	}
}

func TestUpcastGolden(t *testing.T) {
	for _, c := range loadVectors(t).Cases {
		v, err := Upcast(Row{Mission: c.Mission, Seq: c.Seq, Kind: c.Kind, Actor: c.Actor, At: time.UnixMicro(c.AtMicros), Payload: []byte(c.Payload)})
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		got, err := v.Canonical()
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		if string(got) != c.Canonical {
			t.Errorf("%s: canonical bytes differ\n got %s\nwant %s", c.Name, got, c.Canonical)
		}
		if v.SchemaVersion != 2 || v.TenantID != PersonalTenant {
			t.Errorf("%s: schema or tenant: %+v", c.Name, v)
		}
	}
}

// An upcast value never carries a verified issuer, whatever the payload says.
func TestLegacyIssuer(t *testing.T) {
	payload := `{"kind":"MISSION_RELEASED","mission":"m_a","seq":1,"issuer":"goodqa","principal":"alice","reason":"x"}`
	v, err := Upcast(Row{Mission: "m_a", Seq: 1, Kind: "MISSION_RELEASED", Actor: "op:mallory", At: time.UnixMicro(1_762_646_400_000_000), Payload: []byte(payload)})
	if err != nil {
		t.Fatal(err)
	}
	if v.Principal.Issuer != LegacyIssuer || v.Principal.Subject != "op:mallory" {
		t.Fatalf("principal %+v", v.Principal)
	}
	got, _ := v.Canonical()
	if strings.Contains(strings.ReplaceAll(string(got), `"principal":{"issuer":"legacy-unverified","subject":"op:mallory"}`, ""), `"issuer":"goodqa"`) && !strings.Contains(string(got), `"payload"`) {
		t.Fatal("payload leaked into the envelope")
	}
}

func TestUpcastRefusals(t *testing.T) {
	at := time.UnixMicro(1_762_646_400_000_000)
	cases := []struct {
		name string
		row  Row
		want error
	}{
		{"type mismatch", Row{Mission: "m_a", Seq: 1, Kind: "ATTEMPT_STARTED", Actor: "a", At: at, Payload: []byte(`{"kind":"ATTEMPT_STARTED","mission":"m_a","seq":1,"fence":"not a number"}`)}, ErrPayload},
		{"unknown kind", Row{Mission: "m_a", Seq: 1, Kind: "NEW_KIND", Actor: "a", At: at, Payload: []byte(`{"kind":"NEW_KIND","mission":"m_a","seq":1}`)}, ErrUnknownKind},
		{"kind differs from row", Row{Mission: "m_a", Seq: 1, Kind: "DELIVERED", Actor: "a", At: at, Payload: []byte(`{"kind":"MISSION_RELEASED","mission":"m_a","seq":1}`)}, ErrRowMismatch},
		{"mission differs from row", Row{Mission: "m_a", Seq: 1, Kind: "DELIVERED", Actor: "a", At: at, Payload: []byte(`{"kind":"DELIVERED","mission":"m_b","seq":1}`)}, ErrRowMismatch},
		{"seq differs from row", Row{Mission: "m_a", Seq: 2, Kind: "DELIVERED", Actor: "a", At: at, Payload: []byte(`{"kind":"DELIVERED","mission":"m_a","seq":1}`)}, ErrRowMismatch},
		{"not json", Row{Mission: "m_a", Seq: 1, Kind: "DELIVERED", Actor: "a", At: at, Payload: []byte(`nope`)}, ErrPayload},
	}
	for _, c := range cases {
		if _, err := Upcast(c.row); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, err, c.want)
		}
	}
	// Unknown keys never fail.
	if _, err := Upcast(Row{Mission: "m_a", Seq: 1, Kind: "DELIVERED", Actor: "a", At: at, Payload: []byte(`{"kind":"DELIVERED","mission":"m_a","seq":1,"future_field":{"x":1}}`)}); err != nil {
		t.Errorf("unknown key refused: %v", err)
	}
}
