package sched

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fabric/internal/credential"
	"fabric/internal/domain"
	"fabric/internal/m1effect"
)

// goodqa#15966 seam 3: the effect step's source is the redacted copy only.
// The raw stream's result text carries a raw-only marker (the bound token,
// which redaction replaces). Through the real settle and EffectStep path,
// the Delivery holds the diagnosis but never the marker; the ticket, issue
// id, extract paths and bound credentials come from fabd. The raw stream
// directory itself is refused as a source.
func TestM1EffectSourceIsRedactedCopy(t *testing.T) {
	f := newM1Fixture(t, true)
	marker := string(f.tokA)
	result, _ := json.Marshal(map[string]any{"type": "result", "subtype": "success", "session_id": "s1", "uuid": "r1", "num_turns": 1,
		"is_error": false, "result": "diagnosis: sum_to is off by one; raw " + marker, "structured_output": map[string]any{"end": map[string]any{"kind": "checkpoint"}}})
	if err := os.WriteFile(filepath.Join(f.sdir, "stream.jsonl"), []byte(initEv+"\n"+string(result)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	files := filepath.Join(t.TempDir(), "files.json")
	if err := os.WriteFile(files, []byte(`["test.sh","lib.sh"]`+"\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	base := m1effect.RedactedSource{
		IssueID:   func(domain.MissionID) (string, error) { return "0b6f9d4e-8a0c-4c1e-9f55-3c2d7a1b9e10", nil },
		Ticket:    func(context.Context, domain.MissionID) (string, error) { return "goodqa#15966", nil },
		FilesJSON: func(domain.AttemptID) string { return files },
	}
	var d m1effect.Delivery
	var perr, rawErr error
	calls := 0
	f.s.cfg.EffectStep = func(ctx context.Context, a EffectAttempt, hb func(context.Context) error) string {
		calls++
		src := base
		src.Creds = a.Creds
		at := m1effect.Attempt{Mission: a.Mission, Attempt: a.Attempt, Fence: a.Fence, RunDir: a.RunDir, Outcome: a.Outcome}
		d, perr = src.Prepare(ctx, at)
		at.RunDir = f.sdir // the raw stream directory
		_, rawErr = src.Prepare(ctx, at)
		return ""
	}
	out := f.s.settleM1(f.mission, f.attempt, 1, f.sdir, f.run, 0, verified, func(cmd *domain.Command, red string, b *credential.Bound) {
		f.s.effectStepCreds(context.Background(), cmd, red, b)
	})
	if calls != 1 || out.Cmd == nil || perr != nil {
		t.Fatalf("effect step calls=%d cmd=%v prepare err=%v", calls, out.Cmd, perr)
	}
	if !strings.Contains(d.Result, "diagnosis: sum_to is off by one") {
		t.Fatalf("result text %q lacks the diagnosis", d.Result)
	}
	if strings.Contains(d.Result, marker[len(marker)-16:]) {
		t.Fatal("the raw-only marker reached the effect source")
	}
	if d.Ticket != "GoodQA goodqa#15966" || d.IssueID != "0b6f9d4e-8a0c-4c1e-9f55-3c2d7a1b9e10" || d.Creds == nil ||
		strings.Join(d.Allow.Paths, ",") != "test.sh,lib.sh" || d.Allow.Mission != string(f.mission) || d.Allow.Attempt != string(f.attempt) {
		t.Fatalf("delivery not from fabd's sources: %+v", d)
	}
	var ce *m1effect.ClassError
	if !errors.As(rawErr, &ce) || ce.Class != m1effect.ClassNotRedacted {
		t.Fatalf("raw stream directory as source: %v, want %s", rawErr, m1effect.ClassNotRedacted)
	}
}
