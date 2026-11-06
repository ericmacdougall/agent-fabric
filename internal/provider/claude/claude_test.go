package claude

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"fabric/internal/host"
)

// The E1 fixture: one real `claude -p` run recorded on 2026-11-06 (EXPERIMENTS.md E1).
func TestParseE1Fixture(t *testing.T) {
	f, err := os.Open("testdata/e1_A.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	run, err := Parse(f, false)
	if err != nil {
		t.Fatal(err)
	}
	if run.SessionID != "8c015f10-7551-42fe-b1db-f552b3e5f672" {
		t.Fatalf("session id %q", run.SessionID)
	}
	if run.Events != 10 || run.ToolCount != 24 || run.MCPServers != 2 || run.Plugins != 5 {
		t.Fatalf("init/events: events=%d tools=%d mcp=%d plugins=%d", run.Events, run.ToolCount, run.MCPServers, run.Plugins)
	}
	if run.Turns != 1 || run.ResultSubtype != "success" || run.End != EndCheckpoint || run.IsError {
		t.Fatalf("result: turns=%d subtype=%s end=%s", run.Turns, run.ResultSubtype, run.End)
	}
	if run.LastTurn.CacheCreationTokens != 24619 || run.LastTurn.CacheReadTokens != 9074 || run.LastTurn.OutputTokens != 4 {
		t.Fatalf("usage: %+v", run.LastTurn)
	}
	if got := run.LastTurn.Context(); got != 33695 {
		t.Fatalf("context floor %d, want 33695 (the E1 34k control)", got)
	}
	if run.TotalCostUSD < 0.09 || run.TotalCostUSD > 0.11 || !run.RateLimited || run.ResultUUID == "" {
		t.Fatalf("cost=%v ratelimited=%v uuid=%q", run.TotalCostUSD, run.RateLimited, run.ResultUUID)
	}
	if len(run.ParseErrors) != 0 {
		t.Fatalf("parse errors: %v", run.ParseErrors)
	}
	// The same stream under a grant that demanded a schema is abandoned.
	f.Seek(0, 0)
	run2, _ := Parse(f, true)
	if run2.End != EndAbandoned {
		t.Fatalf("no structured_output with a schema: want abandoned, got %s", run2.End)
	}
}

func TestParseClassifications(t *testing.T) {
	cases := map[string]struct {
		in   string
		want EndKind
	}{
		"no result":       {`{"type":"system","subtype":"init","session_id":"s"}` + "\n", EndUnknown},
		"error result":    {`{"type":"result","subtype":"error_during_execution","is_error":true,"session_id":"s"}` + "\n", EndFailed},
		"max turns":       {`{"type":"result","subtype":"error_max_turns","session_id":"s"}` + "\n", EndFailed},
		"success":         {`{"type":"result","subtype":"success","session_id":"s","structured_output":{"next":"x"}}` + "\n", EndCheckpoint},
		"garbage then ok": {"not json\n" + `{"type":"result","subtype":"success","session_id":"s"}` + "\n", EndCheckpoint},
		"garbage only":    {"{\n", EndUnknown},
	}
	for name, c := range cases {
		run, err := Parse(strings.NewReader(c.in), false)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if run.End != c.want {
			t.Fatalf("%s: end=%s want %s", name, run.End, c.want)
		}
	}
	// A changed session id mid-stream is recorded, never silently adopted.
	run, _ := Parse(strings.NewReader(`{"type":"system","subtype":"init","session_id":"a"}`+"\n"+`{"type":"result","subtype":"success","session_id":"b"}`+"\n"), false)
	if run.SessionID != "a" || len(run.ParseErrors) != 1 {
		t.Fatalf("session change: id=%s errors=%v", run.SessionID, run.ParseErrors)
	}
	// An oversized line is bounded (R25).
	huge := `{"type":"assistant","session_id":"s","message":{"usage":{"input_tokens":1}},"pad":"` + strings.Repeat("x", MaxLineBytes) + `"}` + "\n"
	run, err := Parse(strings.NewReader(huge), false)
	if err != nil || run.End != EndUnknown || len(run.ParseErrors) == 0 {
		t.Fatalf("oversized line: err=%v end=%s errors=%v", err, run.End, run.ParseErrors)
	}
}

func validGrant(t *testing.T) host.Grant {
	t.Helper()
	dir := t.TempDir()
	return host.Grant{Run: "r_1", Mission: "m_1", Fence: 1, Model: "policy-model-x", Effort: "medium", Rung: "R0",
		TierTableSHA: "abc", Profile: "P1", MaxTurns: 40, MaxBudgetUSD: 2, Wall: 45 * time.Minute,
		Worktree: dir, RunDir: dir, Token: "0123456789abcdef0123"}
}

// AC-15 / R23: the launch line comes from the grant; a grant without a model
// is refused and no argv exists.
func TestArgvFromGrantOnly(t *testing.T) {
	g := validGrant(t)
	l := Launch{Binary: "/usr/bin/claude", Prompt: "checkpoint", SchemaPath: "/x/schema.json", Tools: []string{"Read", "Edit"}, DisallowedTools: []string{"Agent"}}
	argv, env, err := Argv(g, l)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	for _, want := range []string{"--model policy-model-x", "--effort medium", "--max-turns 40", "--max-budget-usd 2.00",
		"--json-schema /x/schema.json", "--tools Read,Edit", "--disallowedTools Agent", "--exclude-dynamic-system-prompt-sections"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("argv lacks %q: %s", want, joined)
		}
	}
	if !contains(env, "CLAUDE_CODE_DISABLE_CLAUDE_MDS=1") || !contains(env, "FAB_RUN="+g.Token) {
		t.Fatalf("env: %v", env)
	}
	g.Model = ""
	if _, _, err := Argv(g, l); err == nil {
		t.Fatal("grant without a model produced a launch line (R23)")
	}
	g = validGrant(t)
	g.Profile = "P2"
	if _, _, err := Argv(g, l); err == nil {
		t.Fatal("profile the host does not offer was accepted")
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// R23: no model identifier anywhere in the fabric source. The only legitimate
// place is the tier table the daemon reads at run time.
func TestNoModelLiteralsInSource(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	// Assembled from parts so this file does not itself contain a model literal.
	names := []string{"son" + "net", "op" + "us", "hai" + "ku", "fab" + "le", "as" + "tra", "gp" + "t-[0-9]"}
	re := regexp.MustCompile(`(?i)\b(` + strings.Join(names, "|") + `)`)
	var hits []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == ".git" || info.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, ".sql") && !strings.HasSuffix(path, ".json") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(b), "\n") {
			if re.MatchString(line) {
				hits = append(hits, path+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) > 0 {
		t.Fatalf("model literals outside the tier table (R23):\n%s", strings.Join(hits, "\n"))
	}
}
