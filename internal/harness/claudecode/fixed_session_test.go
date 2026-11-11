package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fabric/internal/harness"
	"fabric/internal/harness/harnesstest"
)

func fakeFixed(mode string) {
	id := os.Getenv("FIXED_SESSION")
	p := os.Getenv("FIXED_PATH")
	emit(map[string]any{"type": "system", "subtype": "init", "session_id": id, "model": "claude-sonnet-public", "permissionMode": "manual", "tools": []string{"Write", "EndConversation"}, "mcp_servers": []any{}})
	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() {
		return
	}
	if mode == "fixed-unknown" {
		emit(map[string]any{"type": "private-new-field", "public_canary": "must-not-escape"})
		return
	}
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "PreToolUse"} {
		fakeFixedHook(id, event, "fixed")
	}
	input := map[string]string{"file_path": p, "content": "fabric\n"}
	if mode != "fixed-no-callback" {
		emit(map[string]any{"type": "assistant", "session_id": id, "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "public-canary-provider-prose"}, map[string]any{"type": "tool_use", "id": "tool1", "name": "Write", "input": input}}}})
		emit(json.RawMessage(permissionLine(p, "fabric\n", "Write", "")))
		if !sc.Scan() {
			return
		}
		var response struct {
			Response struct {
				Response struct {
					Behavior string `json:"behavior"`
				} `json:"response"`
			} `json:"response"`
		}
		if json.Unmarshal(sc.Bytes(), &response) != nil || response.Response.Response.Behavior != "allow" {
			return
		}
	}
	_ = os.WriteFile(p, []byte("fabric\n"), 0600)
	fakeFixedHook(id, "Stop", mode)
	emit(map[string]any{"type": "result", "subtype": "success", "session_id": id, "is_error": false, "result": "public-canary-result"})
}

func fakeFixedHook(session, event, mode string) {
	id := "hook_" + event
	if mode == "fixed-hook-missing" {
		return
	}
	if mode != "fixed-hook-unbound" {
		emit(map[string]any{"type": "system", "subtype": "hook_started", "session_id": session, "hook_id": id, "hook_event": event, "hook_name": event})
	}
	if mode == "fixed-hook-outstanding" {
		return
	}
	exit, outcome := 0, "success"
	if mode == "fixed-hook-failed" {
		exit, outcome = 1, "error"
	}
	emit(map[string]any{"type": "system", "subtype": "hook_response", "session_id": session, "hook_id": id, "hook_event": event, "hook_name": event, "exit_code": exit, "outcome": outcome, "output": "public-canary-hook-output"})
}

func TestFixedSessionRequiresCallbackAndDoesNotPublishRawFrames(t *testing.T) {
	for _, mode := range []string{"fixed", "fixed-no-callback", "fixed-unknown", "fixed-hook-failed", "fixed-hook-missing", "fixed-hook-unbound", "fixed-hook-outstanding"} {
		t.Run(mode, func(t *testing.T) {
			d := t.TempDir()
			p := filepath.Join(d, "demo.txt")
			fixed := &FixedWrite{Path: p, RequiredHooks: map[string]int{"SessionStart": 1, "UserPromptSubmit": 1, "PreToolUse": 1, "Stop": 1}}
			a := &Adapter{Launch: FixedLauncher([]string{os.Args[0]}), FixedWrite: fixed, SingleTurn: true, StopGrace: time.Second, DrainGrace: 100 * time.Millisecond, DrainHardCap: time.Second}
			rec := &harnesstest.Recorder{}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			s, err := a.Start(ctx, harness.StartSpec{SessionID: "owned-session", Dir: d, Prompt: "public-canary-prompt", Env: map[string]string{modeEnv: mode, "FIXED_SESSION": "owned-session", "FIXED_PATH": p}}, rec)
			if err != nil {
				t.Fatal(err)
			}
			final, err := s.Wait(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if (final.Outcome == harness.OutcomeDone) != (mode == "fixed") {
				t.Fatalf("wrong outcome %+v", final)
			}
			b, _ := json.Marshal(rec.Snapshot())
			if strings.Contains(string(b), "public-canary") || strings.Contains(string(b), "must-not-escape") || strings.Contains(string(b), p) {
				t.Fatal("raw private frame escaped reducer")
			}
			if err := harness.CheckStream(rec.Snapshot()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
