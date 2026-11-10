package codex_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"fabric/internal/control/ingest"
	"fabric/internal/harness"
	"fabric/internal/harness/codex"
	"fabric/internal/harness/harnesstest"
)

func TestLifecycleWhitelistsAndValidates(t *testing.T) {
	var out bytes.Buffer
	rec := &harnesstest.Recorder{}
	const id = "1234567890abcdef1234567890abcdef"
	s, err := codex.NewLifecycleSink(&out, rec, id)
	if err != nil {
		t.Fatal(err)
	}
	em, _ := harness.NewEmitter("codex", id, s, harness.PatternRedactor())
	const canary = "private-prompt-tool-reason-canary"
	_ = em.Message(context.Background(), "user", canary)
	_ = em.ToolCall(context.Background(), canary, canary, []byte(canary), "requested")
	if err := em.Final(context.Background(), harness.Final{Outcome: harness.OutcomeDone, Reason: canary}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), canary) {
		t.Fatal("raw content reached public lifecycle output")
	}
	lines := bytes.Split(bytes.TrimSpace(out.Bytes()), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("lifecycle rows: %d", len(lines))
	}
	for i, line := range lines {
		if _, err := ingest.ValidateLine(line, "codex_demo"); err != nil {
			t.Fatalf("row %d fails normal ingest: %v", i, err)
		}
		var row map[string]any
		_ = json.Unmarshal(line, &row)
		if row["source"] != "codex_demo" || row["kind"] != "owner" {
			t.Fatal("wrong lifecycle attribution")
		}
	}
	if !strings.Contains(string(lines[1]), "completed") {
		t.Fatal("completion missing")
	}
}

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }
func TestLifecycleSinkFailureAndIdentity(t *testing.T) {
	const id = "1234567890abcdef1234567890abcdef"
	s, _ := codex.NewLifecycleSink(shortWriter{}, &harnesstest.Recorder{}, id)
	e := harness.Event{Version: 1, Harness: "codex", Session: id, Seq: 1, Kind: harness.KindMessage}
	if err := s.Emit(context.Background(), e); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write: %v", err)
	}
	if err := s.Emit(context.Background(), e); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal("sink failure was forgotten")
	}
	var out bytes.Buffer
	s, _ = codex.NewLifecycleSink(&out, &harnesstest.Recorder{}, id)
	e.Session = "other"
	if err := s.Emit(context.Background(), e); err == nil || out.Len() != 0 {
		t.Fatal("wrong session projected")
	}
}
