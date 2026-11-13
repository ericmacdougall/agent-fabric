package claude

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// goodqa#15373 D1 AC-6, NEG-5, NEG-8: StreamArgv is Argv with the prompt
// moved to stdin; every other flag (turn and budget caps included) and the
// environment are identical, and the prompt is in no argv element.
func TestStreamArgv(t *testing.T) {
	g := validGrant(t)
	const canary = "PROMPT-CANARY-15373 continue the mission"
	l := ProductionLaunch("/usr/bin/claude", "/x/schema.json", "")
	l.Prompt = canary
	a, ae, err := Argv(g, l)
	if err != nil {
		t.Fatal(err)
	}
	s, se, err := StreamArgv(g, l)
	if err != nil {
		t.Fatal(err)
	}
	want := append([]string{a[0], "-p", "--input-format", "stream-json"}, a[3:]...)
	if !reflect.DeepEqual(s, want) {
		t.Errorf("StreamArgv differs from Argv beyond the prompt:\n got %q\nwant %q", s, want)
	}
	if !reflect.DeepEqual(se, ae) {
		t.Errorf("StreamArgv env differs from Argv env")
	}
	for _, x := range s {
		if strings.Contains(x, "PROMPT-CANARY") {
			t.Fatalf("the prompt is in argv: %q", x)
		}
	}
	joined := strings.Join(s, " ")
	for _, flag := range []string{"--max-turns 40", "--max-budget-usd 2.00", "--output-format stream-json", "--json-schema /x/schema.json"} {
		if !strings.Contains(joined, flag) {
			t.Errorf("StreamArgv lacks %q", flag)
		}
	}
	// a refused grant stays refused
	g.Model = ""
	if _, _, err := StreamArgv(g, l); err == nil {
		t.Errorf("StreamArgv accepted a grant without a model")
	}
}

func TestStreamInput(t *testing.T) {
	b, err := StreamInput("hello \"world\"\nline two")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(b), "\n") || strings.Count(string(b), "\n") != 1 {
		t.Fatalf("want exactly one newline-terminated line, got %q", b)
	}
	var m struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m.Type != "user" || m.Message.Role != "user" || m.Message.Content != "hello \"world\"\nline two" {
		t.Errorf("bad message %+v", m)
	}
	if _, err := StreamInput(""); err == nil {
		t.Errorf("an empty prompt was accepted")
	}
}
