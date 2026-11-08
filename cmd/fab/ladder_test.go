package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"fabric/internal/domain"
	"fabric/internal/metrics"
	"fabric/internal/store"
	"fabric/internal/storetest"
	"fabric/internal/tui"
)

// The ladder logic is tested in internal/ladder. These check the CLI: the
// refusals (no store needed), then record and status against the test database.
func TestLadderMainUsageIsRefusal(t *testing.T) {
	for _, args := range [][]string{nil, {"bogus"}, {"status"}, {"status", "--state", "x"}, {"status", "--state", "x", "--policy", "y", "extra"},
		{"status", "--task", "t"}, {"status", "--task", "t", "--state", "s", "--policy", "p"}, {"record"}, {"record", "--task", "t"},
		{"record", "--event", "f"}, {"record", "--task", "t", "--event", "f", "extra"}, {"record", "--task", "bad task", "--event", "f"},
		{"status", "--task", strings.Repeat("a", 56), "--policy", "p"}} {
		if err := ladderMain(args); exitCode(err) != 3 {
			t.Errorf("args %q: exit %d (%v), want 3", args, exitCode(err), err)
		}
	}
}

func capture(t *testing.T, f func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	ferr := f()
	w.Close()
	os.Stdout = old
	return <-done, ferr
}

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func oraclePolicy(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile("../../internal/ladder/testdata/rung_oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	var o struct {
		Policy json.RawMessage `json:"policy"`
	}
	if err := json.Unmarshal(b, &o); err != nil {
		t.Fatal(err)
	}
	return writeFile(t, dir, "policy.json", string(o.Policy))
}

var (
	fpA  = strings.Repeat("ab", 32)
	fpB  = strings.Repeat("cd", 32)
	hd1  = strings.Repeat("e1", 20)
	rs1  = "aa" + strings.Repeat("0", 62)
	rs2  = "bb" + strings.Repeat("0", 62)
	rs3  = "cc" + strings.Repeat("0", 62)
	rec1 = `{"class":"FAIL","step":"review","fp":"` + fpA + `","commit":"` + hd1 + `","rsha":"` + rs1 + `","src":"token-abc123","launched":true,"counted":true,"spend_micros":5000000,"ids":["RT-1"]}`
	rec2 = `{"class":"FAIL","step":"review","fp":"` + fpB + `","rsha":"` + rs2 + `","src":"review","launched":true,"counted":true,"spend_micros":5000000}`
)

func TestLadderRecordAndStatus(t *testing.T) {
	t.Setenv("FABRIC_DSN", storetest.DSN)
	st := storetest.Open(t)
	_ = st
	dir := t.TempDir()
	pol := oraclePolicy(t, dir)

	out, err := capture(t, func() error {
		return ladderMain([]string{"record", "--task", "gt1", "--event", writeFile(t, dir, "e1.json", rec1)})
	})
	if err != nil || out != "recorded receipt task gt1 seq 1 step review class FAIL replayed=false\n" {
		t.Fatalf("record: (%q, %v)", out, err)
	}
	// A resend of the same file is a replay.
	out, err = capture(t, func() error {
		return ladderMain([]string{"record", "--task", "gt1", "--event", writeFile(t, dir, "e1b.json", rec1)})
	})
	if err != nil || !strings.HasSuffix(out, "replayed=true\n") {
		t.Fatalf("resend: (%q, %v)", out, err)
	}
	// The same receipt_sha256 with another class is a conflict, exit 3 (through the CLI/import path).
	changed := strings.Replace(rec1, `"class":"FAIL"`, `"class":"NOT_COUNTED"`, 1)
	changed = strings.Replace(changed, `"fp":"`+fpA+`",`, "", 1)
	if _, err := capture(t, func() error {
		return ladderMain([]string{"record", "--task", "gt1", "--event", writeFile(t, dir, "e1c.json", changed)})
	}); exitCode(err) != 3 {
		t.Fatalf("conflict: exit %d (%v), want 3", exitCode(err), err)
	}
	if _, err := capture(t, func() error {
		return ladderMain([]string{"record", "--task", "gt1", "--event", writeFile(t, dir, "e2.json", rec2)})
	}); err != nil {
		t.Fatal(err)
	}
	// Refusals store nothing and exit 3.
	for name, body := range map[string]string{
		"unknown field":   `{"class":"PASS","step":"review","rsha":"` + rs3 + `","launched":true,"extra":1}`,
		"two values":      rec2 + rec2,
		"not an object":   `[1]`,
		"unknown class":   `{"class":"MAYBE","step":"review","rsha":"` + rs3 + `"}`,
		"inferred source": `{"step":"approve_top","approval":{"ask":"a_1","src":"inferred: from x","by":"op:eric"}}`,
		"wal source":      `{"step":"approve_top","approval":{"ask":"a_1","src":"wal:/x#w1","by":"op:eric"}}`,
		"ticket source":   `{"step":"approve_top","approval":{"ask":"a_1","src":"ticket:GoodQA goodqa#1","by":"op:eric"}}`,
		"no source":       `{"step":"approve_top","approval":{"ask":"a_1","by":"op:eric"}}`,
		"non-op actor":    `{"step":"approve_top","approval":{"ask":"a_1","src":"user:s/u","by":"chat"}}`,
		"non-hex commit":  `{"class":"FAIL","step":"review","fp":"` + fpA + `","rsha":"` + rs3 + `","commit":"not-a-sha","launched":true}`,
	} {
		if _, err := capture(t, func() error {
			return ladderMain([]string{"record", "--task", "gt1", "--event", writeFile(t, dir, "bad.json", body)})
		}); exitCode(err) != 3 {
			t.Errorf("%s: exit %d (%v), want 3", name, exitCode(err), err)
		}
	}
	// A valid approval from an operator with a user: source is accepted.
	ok := `{"step":"approve_top","approval":{"ask":"a_1","src":"user:s/u \"ok\"","by":"op:eric"}}`
	out, err = capture(t, func() error {
		return ladderMain([]string{"record", "--task", "gt1", "--event", writeFile(t, dir, "ok.json", ok)})
	})
	if err != nil || !strings.HasPrefix(out, "recorded approval task gt1") {
		t.Fatalf("approval: (%q, %v)", out, err)
	}

	// Status from the database (text).
	text, err := capture(t, func() error { return ladderMain([]string{"status", "--task", "gt1", "--policy", pol}) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"task gt1\n", "rung R0\n", "reviewer_rung R1\n", "failures_at_rung 1\n", "top_approved true\n", "receipts 2\n", "launches 2\n",
		"spend_usd 10.000000\n", "decision CONTINUE (advisory until S3; the hook enforces)\n", "fingerprint abababababab fails 1 heads e1e1e1e1e1e1:1 ids RT-1\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("status text lacks %q:\n%s", want, text)
		}
	}
	// NEG-6: no DSN, key or label text from a receipt.
	for _, leak := range []string{storetest.DSN, "postgres://", "token-abc123", "user:s/u", "op:eric", rs1} {
		if leak != "" && strings.Contains(text, leak) {
			t.Errorf("status output leaks %q", leak)
		}
	}
	js, err := capture(t, func() error { return ladderMain([]string{"status", "--task", "gt1", "--policy", pol, "--json"}) })
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Rung     string `json:"rung"`
		Decision string `json:"decision"`
		Advisory bool   `json:"advisory"`
		Receipts int    `json:"receipts"`
	}
	if err := json.Unmarshal([]byte(js), &parsed); err != nil || parsed.Rung != "R0" || parsed.Decision != "CONTINUE" || !parsed.Advisory || parsed.Receipts != 2 {
		t.Fatalf("status json: (%+v, %v)\n%s", parsed, err, js)
	}
	for _, leak := range []string{storetest.DSN, "postgres://", "token-abc123", "user:s/u", "op:eric", rs1} {
		if leak != "" && strings.Contains(js, leak) {
			t.Errorf("status json leaks %q", leak)
		}
	}
	// An unknown task is an empty ladder, not an error.
	empty, err := capture(t, func() error { return ladderMain([]string{"status", "--task", "nosuch", "--policy", pol}) })
	if err != nil || !strings.Contains(empty, "receipts 0\n") || !strings.Contains(empty, "decision CONTINUE") {
		t.Fatalf("empty task: (%q, %v)", empty, err)
	}
}

// NEG-3 through the CLI: at a permission rung the decision is STOP_AND_ASK
// until an operator approval with a user-sourced id is recorded for that task.
func TestLadderStatusStopAndAskUntilApproved(t *testing.T) {
	t.Setenv("FABRIC_DSN", storetest.DSN)
	storetest.Open(t)
	dir := t.TempDir()
	pol := oraclePolicy(t, dir)
	// Climb to the first permission rung of the oracle policy with distinct fingerprints.
	for i := 0; i < 12; i++ {
		fp := fmt.Sprintf("%064x", 1)                            // one fingerprint, a new head each time: each record is a counted failure
		rs := fmt.Sprintf("%08x", i+1) + strings.Repeat("a", 56) // varies in the leading 62 hex: that is the command key
		r := `{"class":"FAIL","step":"review","fp":"` + fp[:64] + `","commit":"` + fmt.Sprintf("%040x", i+1) + `","rsha":"` + rs + `","launched":true}`
		if _, err := capture(t, func() error {
			return ladderMain([]string{"record", "--task", "gt2", "--event", writeFile(t, dir, "r.json", r)})
		}); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}
	text, err := capture(t, func() error { return ladderMain([]string{"status", "--task", "gt2", "--policy", pol}) })
	if err != nil || !strings.Contains(text, "needs_user_permission true") || !strings.Contains(text, "decision STOP_AND_ASK") {
		t.Fatalf("at the permission rung: (%v)\n%s", err, text)
	}
	// An approval on ANOTHER task does not release this one.
	ok := `{"step":"approve_top","approval":{"ask":"a_1","src":"user:s/u","by":"op:eric"}}`
	if _, err := capture(t, func() error {
		return ladderMain([]string{"record", "--task", "other", "--event", writeFile(t, dir, "ok.json", ok)})
	}); err != nil {
		t.Fatal(err)
	}
	text, _ = capture(t, func() error { return ladderMain([]string{"status", "--task", "gt2", "--policy", pol}) })
	if !strings.Contains(text, "decision STOP_AND_ASK") {
		t.Fatalf("a cross-task approval released the task:\n%s", text)
	}
	if _, err := capture(t, func() error {
		return ladderMain([]string{"record", "--task", "gt2", "--event", writeFile(t, dir, "ok.json", ok)})
	}); err != nil {
		t.Fatal(err)
	}
	text, _ = capture(t, func() error { return ladderMain([]string{"status", "--task", "gt2", "--policy", pol}) })
	// Approved: no longer an ask. The climb limit is also reached here, so the answer is STOP.
	if strings.Contains(text, "STOP_AND_ASK") || !strings.Contains(text, "decision STOP (") || !strings.Contains(text, "top_approved true") {
		t.Fatalf("with its own approval:\n%s", text)
	}
}

// PL-10: the status path opens a read-only pool; a write through it fails.
func TestLadderStatusPoolIsReadOnly(t *testing.T) {
	rw := storetest.Open(t)
	// Control: the same statement succeeds on the read-write pool, so the
	// failure below is the read-only path and not a bad statement.
	if _, err := rw.Pool().Exec(context.Background(), `INSERT INTO missions (id, ticket, state, provider) VALUES ('m_rw_control', 't', '{}', 'claude-code')`); err != nil {
		t.Fatalf("control insert on the read-write pool: %v", err)
	}
	pool, err := tui.OpenPool(storetest.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	err = metrics.ReadOnly(context.Background(), pool, 5*time.Second, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO missions (id, ticket, state, provider) VALUES ('m_rw', 't', '{}', 'claude-code')`)
		return err
	})
	if err == nil {
		t.Fatal("a write through the status pool succeeded")
	}
}

// CI-7: nothing but whitespace may follow the record object. dec.More() is false
// for a stray "]" or "}", so the end is checked with Token() == io.EOF.
func TestDecodeLadderRecordRefusesTrailingData(t *testing.T) {
	one := `{"class":"CLAIM","step":"claim","commit":"abcdef0"}`
	if _, err := decodeLadderRecord([]byte(one + " \n\t")); err != nil {
		t.Fatalf("trailing whitespace refused: %v", err)
	}
	for name, tail := range map[string]string{"bracket": "]", "brace": "}", "text": " x", "second object": one, "comma": ",", "null": "null"} {
		if _, err := decodeLadderRecord([]byte(one + tail)); exitCode(usageError{err}) != 3 || err == nil {
			t.Errorf("trailing %s accepted: %v", name, err)
		}
	}
}

// PL-11: a ladder row that is listed at all is a fault and carries a tag.
func TestStatusTableTagsLadderRows(t *testing.T) {
	rows := []store.MissionRow{
		{ID: "m_ladder-t1", Provider: domain.ProviderAnthropicAPI, Execution: domain.ExecAdmitted, Candidate: "NONE", Delivery: "NOT_READY", Ticket: "ladder-receipts", Name: ""},
		{ID: "m_real", Provider: domain.ProviderClaudeCode, Execution: domain.ExecIdle, Candidate: "NONE", Delivery: "NOT_READY", Ticket: "GoodQA goodqa#1", Name: "real"},
	}
	var b strings.Builder
	if err := writeMissions(&b, rows); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	if len(lines) != 3 || !strings.HasSuffix(lines[1], "[LADDER]") || strings.Contains(lines[2], "[LADDER]") {
		t.Fatalf("status table:\n%s", b.String())
	}
}
