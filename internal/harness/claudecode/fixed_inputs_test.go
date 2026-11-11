package claudecode

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func hashInput(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func TestFixedSnapshotPreservesOnlyReviewedMandatorySources(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "policy")
	s := filepath.Join(d, "settings")
	policy := []byte("public required policy marker\n")
	hooks := map[string]any{"PreToolUse": []any{map[string]any{"matcher": "*", "hooks": []any{map[string]any{"type": "command", "command": "python3", "args": []string{"/public/guard.py", "hook"}, "timeout": 30, "onFailure": "block"}}}}}
	h, _ := json.Marshal(hooks)
	config, _ := json.Marshal(map[string]any{"hooks": hooks, "env": map[string]string{"PUBLIC_CANARY": "discarded"}, "enabledPlugins": map[string]bool{"public-plugin": true}})
	if os.WriteFile(p, policy, 0600) != nil || os.WriteFile(s, config, 0600) != nil {
		t.Fatal("fixture write failed")
	}
	id := InputIdentity{PolicySHA: hashInput(policy), HooksSHA: hashInput(h)}
	got, settings, err := FixedInputSnapshot(p, s, id)
	if err != nil || !reflect.DeepEqual(got, policy) {
		t.Fatalf("required policy not preserved %v", err)
	}
	var out map[string]json.RawMessage
	if json.Unmarshal(settings, &out) != nil {
		t.Fatal("bad settings")
	}
	var outHooks map[string]any
	_ = json.Unmarshal(out["hooks"], &outHooks)
	canonical, _ := json.Marshal(outHooks)
	var permissions struct {
		Deny []string `json:"deny"`
		Ask  []string `json:"ask"`
	}
	if json.Unmarshal(out["permissions"], &permissions) != nil || !reflect.DeepEqual(permissions.Ask, []string{"Edit"}) {
		t.Fatal("host write approval lost")
	}
	for _, denied := range permissions.Deny {
		if denied == "Read" {
			t.Fatal("blanket Read rule also blocks the fixed Write target")
		}
	}
	if string(canonical) != string(h) || strings.Contains(string(settings), "PUBLIC_CANARY") || strings.Contains(string(settings), "public-plugin") {
		t.Fatal("hooks changed or discarded settings leaked")
	}
	for _, change := range []string{"policy", "hooks", "import", "duplicate"} {
		t.Run(change, func(t *testing.T) {
			policyNow := policy
			configNow := config
			switch change {
			case "policy":
				policyNow = []byte("changed")
			case "import":
				policyNow = []byte("@/public/unapproved")
			case "hooks":
				configNow = []byte(`{"hooks":{}}`)
			case "duplicate":
				configNow = []byte(`{"hooks":{},"hooks":{}}`)
			}
			_ = os.WriteFile(p, policyNow, 0600)
			_ = os.WriteFile(s, configNow, 0600)
			if _, _, e := FixedInputSnapshot(p, s, id); e == nil {
				t.Fatal("changed input accepted")
			}
		})
	}
}

func TestFixedInvocationHasNoInheritedStartupControls(t *testing.T) {
	env := FixedEnvironment("/public/home")
	if env["USER"] != "eric" {
		t.Fatal("native store account identity absent")
	}
	if env["AGENT_HARNESS_TASK_OUTPUT_PRUNE"] != "off" {
		t.Fatal("detached maintenance enabled")
	}
	if env["DISABLE_AUTOUPDATER"] != "1" || env["ENABLE_CLAUDEAI_MCP_SERVERS"] != "false" {
		t.Fatal("optional update/connector discovery enabled")
	}
	for _, k := range []string{"CLAUDE_CODE_DISABLE_CLAUDE_MDS", "CLAUDE_CODE_DISABLE_AUTO_MEMORY", "CLAUDE_CODE_DISABLE_ATTACHMENTS"} {
		if env[k] != "1" {
			t.Fatal("missing disable")
		}
	}
	for _, k := range []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "NODE_OPTIONS", "ANTHROPIC_BASE_URL", "CLAUDE_CONFIG_DIR", "CLAUDE_CODE_SIMPLE"} {
		if _, ok := env[k]; ok {
			t.Fatal("unsafe env")
		}
	}
	argv, err := FixedArguments("12345678-1234-1234-1234-123456789abc", "/public/settings", "/public/policy", "/public/mcp")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(argv, " ")
	for _, want := range []string{"--restricted", "--tools Write", "--permission-mode manual", "--permission-prompt-tool stdio", "--append-system-prompt-file", "--no-session-persistence"} {
		if !strings.Contains(text, want) {
			t.Fatal("missing control", want)
		}
	}
	for _, bad := range []string{"--bare", "--allowedTools", "--resume", "--continue", "--bypass", "--system-prompt-file"} {
		if strings.Contains(text, bad) {
			t.Fatal("unsafe control", bad)
		}
	}
	if _, err := FixedArguments("unowned", "/public/settings", "/public/policy", "/public/mcp"); err == nil {
		t.Fatal("unowned identity")
	}
}

func TestFixedOwnerBindsNativeAccountToUIDAndHome(t *testing.T) {
	identity := &user.User{Username: "eric", Uid: "501", HomeDir: "/Users/eric"}
	if !fixedOwnerMatches("/Users/eric", identity, 501, 501, true) {
		t.Fatal("valid owner refused")
	}
	for _, test := range []struct {
		home     string
		identity user.User
		uid      int
		homeUID  uint32
		real     bool
	}{
		{"/Users/eric", *identity, 501, 502, true},
		{"/Users/eric", *identity, 502, 502, true},
		{"/Users/other", *identity, 501, 501, true},
		{"/Users/eric", user.User{Username: "other", Uid: "501", HomeDir: "/Users/eric"}, 501, 501, true},
		{"/Users/eric", *identity, 501, 501, false},
	} {
		if fixedOwnerMatches(test.home, &test.identity, test.uid, test.homeUID, test.real) {
			t.Fatal("foreign user/home accepted")
		}
	}
	if CheckFixedOwner(t.TempDir()) == nil {
		t.Fatal("arbitrary workspace accepted as native account home")
	}
}

func TestFixedSnapshotRefusesUnreviewedManagedCache(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "policy")
	s := filepath.Join(d, "settings")
	cache := filepath.Join(d, "remote-settings.json")
	policy := []byte("public required")
	hooks := []byte(`{"Stop":[]}`)
	_ = os.WriteFile(p, policy, 0600)
	_ = os.WriteFile(s, []byte(`{"hooks":{"Stop":[]}}`), 0600)
	id := InputIdentity{PolicySHA: hashInput(policy), HooksSHA: hashInput(hooks)}
	for _, value := range []string{`{}`, `{"env":{"PUBLIC_CANARY":"not approved"}}`, `null`, `{"x":1,"x":2}`} {
		_ = os.WriteFile(cache, []byte(value), 0600)
		_, _, err := FixedInputSnapshot(p, s, id)
		if (err == nil) != (value == `{}`) {
			t.Fatal("wrong managed-cache decision")
		}
		if err != nil && strings.Contains(err.Error(), "PUBLIC_CANARY") {
			t.Fatal("cache reflected")
		}
	}
}
