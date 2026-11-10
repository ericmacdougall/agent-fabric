package codex_test

import (
	"encoding/json"
	"fabric/internal/harness/codex"
	"path/filepath"
	"testing"
)

func validHookMetadata(home, cwd string) []byte {
	var hooks []map[string]any
	add := func(key, event, script, args string, timeout int) {
		hooks = append(hooks, map[string]any{"key": filepath.Join(home, ".codex", "hooks.json") + ":" + key, "eventName": event, "source": "user", "sourcePath": filepath.Join(home, ".codex", "hooks.json"), "enabled": true, "trustStatus": "trusted", "handlerType": "command", "async": false, "command": "python3 " + filepath.Join(home, ".codex", "agent-harness", script) + args, "timeoutSec": timeout})
	}
	add("pre_tool_use:0:0", "preToolUse", "wal.py", " hook", 10)
	add("pre_tool_use:1:0", "preToolUse", "commit_policy.py", " hook", 60)
	add("pre_tool_use:2:0", "preToolUse", "merge_gate.py", " hook || exit 2", 60)
	add("pre_tool_use:3:0", "preToolUse", "command_guard_hook.py", " hook", 30)
	add("pre_tool_use:4:0", "preToolUse", "kill_guard.py", " hook", 30)
	add("pre_tool_use:5:0", "preToolUse", "heavy_job_guard.py", "", 30)
	add("session_start:0:0", "sessionStart", "wal.py", " hook", 10)
	add("user_prompt_submit:0:0", "userPromptSubmit", "wal.py", " hook", 10)
	add("subagent_start:0:0", "subagentStart", "wal.py", " hook", 10)
	add("stop:0:0", "stop", "wal.py", " hook", 10)
	add("interrupt:0:0", "interrupt", "wal.py", " hook", 1)
	b, _ := json.Marshal(map[string]any{"data": []any{map[string]any{"cwd": cwd, "errors": []any{}, "warnings": []any{}, "hooks": hooks}}})
	return b
}
func TestMandatoryHookTrustGate(t *testing.T) {
	const home = "/home/owned"
	const cwd = "/owned/work"
	good := validHookMetadata(home, cwd)
	if err := codex.CheckHooks(good, home, cwd); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"enabled", "trustStatus", "command", "source", "sourcePath", "eventName", "timeoutSec", "async"} {
		var d map[string]any
		_ = json.Unmarshal(good, &d)
		h := d["data"].([]any)[0].(map[string]any)["hooks"].([]any)[3].(map[string]any)
		switch field {
		case "enabled":
			h[field] = false
		case "async":
			h[field] = true
		case "timeoutSec":
			h[field] = 600
		default:
			h[field] = "unapproved-canary"
		}
		b, _ := json.Marshal(d)
		if err := codex.CheckHooks(b, home, cwd); err == nil {
			t.Errorf("accepted changed %s", field)
		}
	}
}
