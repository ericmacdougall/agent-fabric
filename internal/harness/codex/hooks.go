package codex

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
)

// AllowedDemoModel rejects denied/unknown model families. Catalog support is
// checked separately; provider refusal is returned once without a fallback.
func AllowedDemoModel(s string) bool { return s == "gpt-6-luna" || s == "gpt-6.1-sol" }

// CheckHooks validates every actual registration, including runtime trust.
// Raw commands, warnings and source paths never enter error messages/events.
func CheckHooks(raw json.RawMessage, home, cwd string) error {
	type hook struct {
		Key, EventName, HandlerType, Command, Source, SourcePath, TrustStatus string
		Enabled                                                               bool
		Async                                                                 bool
		TimeoutSec                                                            int
	}
	var result struct {
		Data []struct {
			Cwd      string
			Errors   []json.RawMessage
			Warnings []json.RawMessage
			Hooks    []hook
		}
	}
	if json.Unmarshal(raw, &result) != nil || len(result.Data) != 1 {
		return errors.New("codex: hook metadata unavailable")
	}
	e := result.Data[0]
	if e.Cwd != cwd || len(e.Errors) > 0 || len(e.Warnings) > 0 || len(e.Hooks) != 11 {
		return errors.New("codex: hook source closure mismatch")
	}
	prefix := filepath.Join(home, ".codex", "agent-harness")
	want := map[string]struct {
		event, command string
		timeout        int
	}{}
	add := func(key, event, script, args string, timeout int) {
		want["/"+key] = struct {
			event, command string
			timeout        int
		}{event, "python3 " + filepath.Join(prefix, script) + args, timeout}
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
	seen := map[string]bool{}
	source := filepath.Join(home, ".codex", "hooks.json")
	for _, h := range e.Hooks {
		key := "/" + strings.TrimPrefix(h.Key, source+":")
		w, ok := want[key]
		if !ok || seen[key] || !h.Enabled || h.TrustStatus != "trusted" || h.HandlerType != "command" || h.Async || h.Source != "user" || h.SourcePath != source || h.EventName != w.event || h.Command != w.command || h.TimeoutSec != w.timeout {
			return errors.New("codex: mandatory hook missing, changed or untrusted")
		}
		seen[key] = true
	}
	return nil
}
