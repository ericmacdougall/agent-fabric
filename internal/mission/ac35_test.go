package mission

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// AC-35(f), spec v16: the gate allows only the exact "./test.sh". A
// compound command, an interpreter running the script, and the script
// followed by a newline and another command are all refused.
func TestBashGateRefusesCompoundAndInterpreter(t *testing.T) {
	if ok, why := BashGate(hookInput(TestCommand)); !ok {
		t.Fatalf("%s refused: %s (the refusals below would be vacuous)", TestCommand, why)
	}
	for _, c := range []string{"./test.sh; id", "bash ./test.sh", "sh test.sh", "./test.sh\nid"} {
		var errb bytes.Buffer
		if ok, _ := BashGate(hookInput(c)); ok {
			t.Errorf("%q allowed", c)
		}
		if code := RunBashGate(bytes.NewReader(hookInput(c)), &errb); code != 2 {
			t.Errorf("%q: hook exit %d, want 2 (block)", c, code)
		}
	}
}

// AC-35(f): the gate never echoes the tool input, so a command carrying a
// token form (built at run time) cannot reach the hook's output; and the
// hook entry fabd writes passes no environment and no arguments: it is
// the gate's absolute path alone, with onFailure block. (Claude Code's
// own child environment for the hook is NEG-18, checked live.)
func TestBashGateEnvAndOutput(t *testing.T) {
	tok := strings.Join([]string{"sk", "ant", "oat01", strings.Repeat("Qz7", 12)}, "-")
	for _, c := range []string{"echo " + tok, "./test.sh " + tok, TestCommand} {
		in := hookInput(c)
		var errb bytes.Buffer
		RunBashGate(bytes.NewReader(in), &errb)
		out := errb.String()
		if strings.Contains(out, tok) || strings.Contains(out, "Qz7Qz7") || (c != TestCommand && strings.Contains(out, c)) {
			t.Errorf("%q: the hook output echoes the tool input: %q", c, out)
		}
		if strings.Contains(out, "run the test") {
			t.Errorf("%q: the hook output echoes the tool input description", c)
		}
	}
	b, err := Settings("/srv/fabric/attempts/r_x_1/worktree", "/srv/fabric/hooks")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Hooks map[string][]struct {
			Matcher string           `json:"matcher"`
			Hooks   []map[string]any `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	pre := doc.Hooks["PreToolUse"]
	if len(pre) != 1 || len(pre[0].Hooks) != 1 {
		t.Fatalf("hook entries %+v", pre)
	}
	h := pre[0].Hooks[0]
	for k := range h {
		switch k {
		case "type", "command", "timeout", "onFailure":
		default:
			t.Errorf("hook entry carries %q (no env, no args)", k)
		}
	}
	if h["command"] != "/srv/fabric/hooks/"+BashGateName || h["onFailure"] != "block" {
		t.Errorf("hook entry %v", h)
	}
	if bytes.Contains(b, []byte("sk-ant")) || bytes.Contains(b, []byte("CLAUDE_CODE_OAUTH")) {
		t.Error("the settings carry a token form or the token variable")
	}
}
