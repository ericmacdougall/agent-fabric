package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

func mk(idx int, role, stop string, blocks ...string) Message {
	var raw []json.RawMessage
	for _, b := range blocks {
		raw = append(raw, json.RawMessage(b))
	}
	c, _ := json.Marshal(raw)
	return Message{Idx: idx, Role: role, Content: c, StopReason: stop}
}

const (
	txt  = `{"type":"text","text":"t"}`
	cmp  = `{"type":"compaction","content":"SUM"}`
	cmpN = `{"type":"compaction","content":null}`
	thk  = `{"type":"thinking","thinking":"hm","signature":"s"}`
)

func TestRequestMessagesServerOmitsPrefix(t *testing.T) {
	msgs := []Message{mk(0, "user", "", txt), mk(1, "assistant", "tool_use", txt), mk(2, "user", "", txt), mk(3, "assistant", "tool_use", cmp, txt), mk(4, "user", "", txt)}
	for name, c := range map[string]struct {
		cfg  CompactionConfig
		want int
	}{
		"server omits":   {CompactionConfig{Mode: CompactServer}, 2},
		"keep_prefix":    {CompactionConfig{Mode: CompactServer, KeepPrefix: true}, 5},
		"off":            {CompactionConfig{}, 5},
		"client ignores": {CompactionConfig{Mode: CompactClient}, 5},
	} {
		l := &Loop{c: Config{Compaction: c.cfg}}
		got, err := l.requestMessages(msgs, nil)
		if err != nil || len(got) != c.want {
			t.Fatalf("%s: %d messages, want %d (%v)", name, len(got), c.want, err)
		}
	}
	// a null summary is not a compaction: nothing is omitted
	null := []Message{mk(0, "user", "", txt), mk(1, "assistant", "end_turn", cmpN, txt), mk(2, "user", "", txt)}
	l := &Loop{c: Config{Compaction: CompactionConfig{Mode: CompactServer}}}
	if got, _ := l.requestMessages(null, nil); len(got) != 3 {
		t.Fatalf("a null compaction block omitted history: %d", len(got))
	}
}

func TestRequestMessagesClientSummaryAndStrip(t *testing.T) {
	msgs := []Message{mk(0, "user", "", txt), mk(1, "assistant", "tool_use", thk, txt), mk(2, "user", "", txt),
		mk(3, "assistant", "tool_use", thk, txt), mk(4, "user", "", txt), mk(5, "assistant", "model_context_window_exceeded", txt)}
	comps := []Compaction{{Idx: 6, FirstKeptIdx: 3, Summary: "THE SUMMARY"}}
	l := &Loop{c: Config{Compaction: CompactionConfig{Mode: CompactClient}}}
	got, err := l.requestMessages(msgs, comps)
	if err != nil || len(got) != 3 || got[0].Role != "user" || !strings.Contains(string(got[0].Content[0]), "THE SUMMARY") || got[1].Role != "assistant" || got[2].Role != "user" {
		t.Fatalf("%+v %v", got, err)
	}
	if !strings.Contains(string(got[1].Content[0]), "thinking") {
		t.Fatal("thinking was stripped without the grant opting in")
	}
	l.c.Compaction.StripThinking = true
	long := []Message{mk(0, "user", "", txt), mk(1, "assistant", "tool_use", thk, txt), mk(2, "user", "", txt), mk(3, "assistant", "tool_use", thk, txt), mk(4, "user", "", txt)}
	got, _ = l.requestMessages(long, nil)
	if strings.Contains(string(got[1].Content[0]), "thinking") || !strings.Contains(string(got[3].Content[0]), "thinking") {
		t.Fatalf("strip must drop thinking from earlier assistant messages and keep the last: %+v", got)
	}
}
