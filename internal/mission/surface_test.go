package mission

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fabric/internal/host"
	"fabric/internal/provider/claude"
)

func hookInput(cmd string) []byte {
	b, _ := json.Marshal(map[string]any{
		"session_id": "s", "transcript_path": "/x", "cwd": "/wt", "hook_event_name": "PreToolUse",
		"tool_name": "Bash", "tool_input": map[string]any{"command": cmd, "description": "run the test"},
	})
	return b
}

// gateCases is shared with the broken-variant check below.
var refusedCommands = []string{
	"./test.sh x", "./test.sh; id", "./test.sh && cat /proc/self/environ", "./test.sh | tee /dev/stderr",
	"./test.sh $(cat /proc/self/environ)", "./test.sh `env`", "cat x", "ls", "env", "bash -c ./test.sh",
	"ADD=1 ./test.sh", "./test.sh ", " ./test.sh", "./test.sh\n", "./test.sh\nenv", "./test.sh\t", "test.sh",
	"/bin/sh ./test.sh", "./test.sh#", "./Test.sh", "./test.sh\x00", "",
}

// AC-24 (owner ruling R1): the PreToolUse gate allows exactly ./test.sh.
func TestBashGate(t *testing.T) {
	if ok, why := BashGate(hookInput("./test.sh")); !ok {
		t.Fatalf("./test.sh refused: %s", why)
	}
	for _, c := range refusedCommands {
		if ok, _ := BashGate(hookInput(c)); ok {
			t.Errorf("%q allowed", c)
		}
	}
	for name, in := range map[string]string{
		"malformed json":        `{"tool_name":"Bash",`,
		"not an object":         `["./test.sh"]`,
		"empty":                 ``,
		"trailing data":         string(hookInput("./test.sh")) + `{}`,
		"other tool":            `{"hook_event_name":"PreToolUse","tool_name":"Read","tool_input":{"command":"./test.sh"}}`,
		"missing tool_name":     `{"hook_event_name":"PreToolUse","tool_input":{"command":"./test.sh"}}`,
		"case-variant key":      `{"hook_event_name":"PreToolUse","Tool_Name":"Bash","tool_input":{"command":"./test.sh"}}`,
		"missing event":         `{"tool_name":"Bash","tool_input":{"command":"./test.sh"}}`,
		"other event":           `{"hook_event_name":"PostToolUse","tool_name":"Bash","tool_input":{"command":"./test.sh"}}`,
		"missing tool_input":    `{"hook_event_name":"PreToolUse","tool_name":"Bash"}`,
		"missing command":       `{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{}}`,
		"null command":          `{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":null}}`,
		"array command":         `{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":["./test.sh"]}}`,
		"duplicate command":     `{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"env","command":"./test.sh"}}`,
		"duplicate tool":        `{"hook_event_name":"PreToolUse","tool_name":"Read","tool_name":"Bash","tool_input":{"command":"./test.sh"}}`,
		"background":            `{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"./test.sh","run_in_background":true}}`,
		"background not a bool": `{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"./test.sh","run_in_background":"no"}}`,
	} {
		if ok, _ := BashGate([]byte(in)); ok {
			t.Errorf("%s: allowed", name)
		}
	}
	if ok, _ := BashGate(append(hookInput("./test.sh"), bytes.Repeat([]byte(" "), maxHookInput)...)); ok {
		t.Error("oversized input allowed")
	}
	// Exit codes: 0 allows, 2 blocks with a reason on stderr.
	var errb bytes.Buffer
	if code := RunBashGate(bytes.NewReader(hookInput("./test.sh")), &errb); code != 0 || errb.Len() != 0 {
		t.Errorf("allow: exit %d, stderr %q", code, errb.String())
	}
	errb.Reset()
	if code := RunBashGate(bytes.NewReader(hookInput("ls")), &errb); code != 2 || !strings.Contains(errb.String(), "refused") {
		t.Errorf("refuse: exit %d, stderr %q", code, errb.String())
	}
}

// The refused table is not vacuous: a prefix-matching gate (the broken
// variant) would allow some of it.
func TestBashGateTableCatchesPrefixMatch(t *testing.T) {
	prefixGate := func(cmd string) bool { return strings.HasPrefix(cmd, TestCommand) }
	trimGate := func(cmd string) bool { return strings.TrimSpace(cmd) == TestCommand }
	caughtPrefix, caughtTrim := 0, 0
	for _, c := range refusedCommands {
		if prefixGate(c) {
			caughtPrefix++
		}
		if trimGate(c) {
			caughtTrim++
		}
	}
	if caughtPrefix == 0 || caughtTrim == 0 {
		t.Fatalf("the refused table would not catch a broken gate (prefix %d, trim %d)", caughtPrefix, caughtTrim)
	}
}

func testConfig(t *testing.T) *Config {
	t.Helper()
	d := validDoc("/srv/fabric/missions/m_canary/brief.md")
	b, _ := json.Marshal(d)
	c, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// AC-24: the settings document pins the surface.
func TestSettingsDocument(t *testing.T) {
	wt := "/srv/fabric/work/attempts/r_canary_1/wt"
	b, err := Settings(wt, "/srv/fabric/hooks")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Permissions struct {
			DefaultMode  string   `json:"defaultMode"`
			Allow        []string `json:"allow"`
			Deny         []string `json:"deny"`
			BlockOutside bool     `json:"blockReadsOutsideWorkingDirectories"`
			DisableByp   string   `json:"disableBypassPermissionsMode"`
			Additional   []string `json:"additionalDirectories"`
		} `json:"permissions"`
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct{ Type, Command string }
		} `json:"hooks"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	p := doc.Permissions
	if strings.Join(p.Allow, " ") != "Bash(./test.sh) Read(//srv/fabric/work/attempts/r_canary_1/wt/**)" {
		t.Errorf("allow = %q", p.Allow)
	}
	for _, a := range p.Allow {
		if strings.HasPrefix(a, "Bash") && a != "Bash(./test.sh)" {
			t.Errorf("extra Bash allow %q", a)
		}
	}
	deny := strings.Join(p.Deny, "\n")
	for _, want := range []string{"Read(//proc/**)", "Read(//sys/**)", "Bash(env *)", "Bash(printenv *)", "Bash(curl *)", "Bash(sh *)", "Bash(bash *)", "Bash(cat *)", "Bash(ls *)"} {
		if !strings.Contains(deny, want+"\n") && !strings.HasSuffix(deny, want) {
			t.Errorf("deny lacks %s", want)
		}
	}
	for _, d := range p.Deny {
		if strings.HasPrefix(d, "Bash(./test.sh") || d == "Bash" || d == "Bash(*)" {
			t.Errorf("deny %q would remove the test command", d)
		}
	}
	if p.DefaultMode != "dontAsk" || !p.BlockOutside || p.DisableByp != "disable" || p.Additional != nil {
		t.Errorf("permissions %+v", p)
	}
	pre := doc.Hooks["PreToolUse"]
	if len(doc.Hooks) != 1 || len(pre) != 1 || pre[0].Matcher != "Bash" || len(pre[0].Hooks) != 1 ||
		pre[0].Hooks[0].Type != "command" || pre[0].Hooks[0].Command != "/srv/fabric/hooks/fab-bash-gate" {
		t.Errorf("hooks %+v", doc.Hooks)
	}
	for _, bad := range []string{"relative/wt", "/srv/wt*", "/srv/w t", "/srv/../wt", "/srv/wt/", "/srv/[wt]", "/"} {
		if _, err := Settings(bad, "/srv/fabric/hooks"); err == nil {
			t.Errorf("worktree %q accepted", bad)
		}
	}
}

// AC-24: the argv carries --tools Read,Grep,Glob,Bash, the settings file,
// the brief through --append-system-prompt-file and dontAsk; and the
// settings file passes the launch's own settings check (hook under
// FABRIC_HOOKS_DIR, file not worker-writable).
func TestM1LaunchArgv(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	hooks := filepath.Join(dir, "hooks")
	os.Mkdir(hooks, 0o755)
	os.WriteFile(filepath.Join(hooks, BashGateName), []byte("#!/bin/sh\n"), 0o755)
	wt := filepath.Join(dir, "wt")
	os.Mkdir(wt, 0o555)
	settings := filepath.Join(dir, "settings.json")
	b, err := Settings(wt, hooks)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(settings, b, 0o444)
	managed := filepath.Join(dir, "managed")
	os.Mkdir(managed, 0o755)

	c := testConfig(t)
	base := claude.ProductionLaunch("/usr/bin/claude", "", "")
	base.Prompt = "checkpoint"
	base.ManagedRoot = managed
	base.Tools = []string{"Read", "Edit", "Write"}
	base.PermissionMode = "acceptEdits"
	l := c.ApplyLaunch(base, settings, hooks)
	g := host.Grant{Run: "r_canary_1", Mission: "m_canary", Fence: 1, Model: "m", Effort: "low", Rung: "r", TierTableSHA: "t",
		ConfigSHA: "n/a", Profile: "P0", MaxTurns: 999, MaxBudgetUSD: 999, Wall: 24 * time.Hour, RunDir: dir, Token: "r_canary_1-1-0123456789abcdef0123"}
	if err := c.ApplyGrant(&g, wt); err != nil {
		t.Fatal(err)
	}
	argv, _, err := claude.Argv(g, l)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, "\x00")
	for _, want := range []string{
		"--tools\x00Read,Grep,Glob,Bash", "--settings\x00" + settings, "--append-system-prompt-file\x00/srv/fabric/missions/m_canary/brief.md",
		"--permission-mode\x00dontAsk", "--max-turns\x0040", "--max-budget-usd\x002.00",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv lacks %q", strings.ReplaceAll(want, "\x00", " "))
		}
	}
	for _, bad := range []string{"--allowedTools", "--allowed-tools", "--dangerously-skip-permissions", "--add-dir", "Edit", "Write", "acceptEdits", "bypassPermissions"} {
		for _, a := range argv {
			if a == bad || strings.Contains(a, bad+",") || strings.HasSuffix(a, ","+bad) {
				t.Errorf("argv carries %q", bad)
			}
		}
	}
	if g.Wall != 45*time.Minute || g.Worktree != wt {
		t.Errorf("grant wall %v worktree %q", g.Wall, g.Worktree)
	}
	// R3: the M1 path refuses P1.
	g.Profile = "P1"
	if err := c.ApplyGrant(&g, wt); err == nil {
		t.Error("P1 accepted on the M1 path")
	}
}
