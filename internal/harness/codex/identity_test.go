package codex

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestFrameTurn(t *testing.T) {
	cases := []struct {
		name       string
		raw        string
		completion bool
		want       string
	}{
		{"content with turnId", `{"threadId":"t","turnId":"u1"}`, false, "u1"},
		{"content without turnId", `{"threadId":"t"}`, false, ""},
		{"content with empty turnId", `{"turnId":""}`, false, ""},
		{"content with non-string turnId", `{"turnId":7}`, false, ""},
		{"completion with turn.id", `{"turn":{"id":"u1"}}`, true, "u1"},
		{"completion without turn", `{"turnId":"u1"}`, true, ""},
		{"completion with empty turn.id", `{"turn":{"id":""}}`, true, ""},
		{"completion with agreeing ids", `{"turnId":"u1","turn":{"id":"u1"}}`, true, "u1"},
		{"completion with disagreeing ids", `{"turnId":"u1","turn":{"id":"u2"}}`, true, ""},
		{"duplicate turnId", `{"turnId":"u1","turnId":"u2"}`, false, ""},
		{"duplicate nested id", `{"turn":{"id":"u1","id":"u2"}}`, true, ""},
		{"not an object", `["turnId"]`, false, ""},
		{"trailing data", `{"turnId":"u1"} {"turnId":"u2"}`, false, ""},
		{"truncated", `{"turnId":"u1"`, false, ""},
	}
	for _, c := range cases {
		if got := frameTurn(json.RawMessage(c.raw), c.completion); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestLineReaderBoundsLines(t *testing.T) {
	in := "short\n" + strings.Repeat("x", 200) + "\r\nafter\nlast-without-newline"
	lr := newLineReader(strings.NewReader(in), 50)
	var got []string
	var over []bool
	for {
		l, err := lr.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, string(l.data))
		over = append(over, l.over)
	}
	want := []string{"short", "", "after", "last-without-newline"}
	if len(got) != len(want) {
		t.Fatalf("lines %q", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d: %q, want %q", i, got[i], want[i])
		}
	}
	if !over[1] || over[0] || over[2] || over[3] {
		t.Errorf("over flags %v", over)
	}
}

func TestLineReaderOversizeDoesNotBuffer(t *testing.T) {
	// 10 MiB line against a 1 KiB bound: only the flag and the size survive.
	lr := newLineReader(io.MultiReader(strings.NewReader(strings.Repeat("a", 10<<20)), strings.NewReader("\nnext\n")), 1024)
	l, err := lr.next()
	if err != nil || !l.over || l.data != nil || l.size <= 10<<20 {
		t.Fatalf("oversize line: %+v %v", l, err)
	}
	l, err = lr.next()
	if err != nil || string(l.data) != "next" {
		t.Fatalf("line after oversize: %+v %v", l, err)
	}
}

func TestToolInfo(t *testing.T) {
	cases := []struct {
		item   string
		name   string
		status string
		ok     bool
	}{
		{`{"type":"commandExecution","command":"ls","status":"completed"}`, "command", "ok", true},
		{`{"type":"commandExecution","command":"ls","status":"failed"}`, "command", "error", true},
		{`{"type":"commandExecution","command":"ls","status":"declined"}`, "command", "denied", true},
		{`{"type":"fileChange","changes":[],"status":"inProgress"}`, "file_change", "requested", true},
		{`{"type":"mcpToolCall","server":"s","tool":"t","arguments":{},"status":"completed"}`, "mcp:s/t", "ok", true},
		{`{"type":"webSearch","query":"q"}`, "web_search", "requested", true},
		{`{"type":"agentMessage"}`, "", "", false},
		{`{"type":"mystery"}`, "", "", false},
	}
	for _, c := range cases {
		var item map[string]any
		if err := json.Unmarshal([]byte(c.item), &item); err != nil {
			t.Fatal(err)
		}
		name, _, status, ok := toolInfo(item)
		if name != c.name || status != c.status || ok != c.ok {
			t.Errorf("%s: got %q %q %v", c.item, name, status, ok)
		}
	}
}
