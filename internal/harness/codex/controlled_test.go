package codex_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"fabric/internal/harness"
	"fabric/internal/harness/codex"
	"fabric/internal/harness/harnesstest"
)

func TestFinalFollowsOwnedGroupCleanup(t *testing.T) {
	a := adapter(t, "child_exit")
	a.RequestTimeout = 300 * time.Millisecond
	spec := startSpec(t, "go")
	s, rec := begin(t, a, spec)
	if !rec.WaitFor(5*time.Second, func(e []harness.Event) bool { return hasText(e, "pid:") }) {
		t.Fatal("child pid was not reported")
	}
	pid := pidFrom(t, rec.Snapshot())
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	f := finish(t, s)
	if f.Outcome != harness.OutcomeFailed {
		t.Fatalf("unanswered parent: %+v", f)
	}
	if liveChild(pid) {
		t.Fatal("Final was emitted while the owned child was still alive")
	}
}
func liveChild(pid int) bool {
	if !alive(pid) {
		return false
	}
	// Linux may retain an orphan zombie until the bounded supervisor reaps it.
	b, e := os.ReadFile(filepath.Join("/proc", fmtInt(pid), "stat"))
	if e == nil {
		i := strings.LastIndex(string(b), ") ")
		return i < 0 || len(b) <= i+2 || b[i+2] != 'Z'
	}
	return true
}
func fmtInt(n int) string { return strconv.Itoa(n) }

func TestFileEditOnlyConfigClosure(t *testing.T) {
	good := codex.FileEditSettings()
	if err := codex.CheckFileEditConfig(good); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"features.shell_tool", "features.hooks", "features.apps", "features.multi_agent", "memories.use_memories", "shell_environment_policy.inherit", "shell_environment_policy.set", "allow_login_shell"} {
		b, _ := json.Marshal(good)
		var bad map[string]any
		_ = json.Unmarshal(b, &bad)
		parts := strings.Split(path, ".")
		m := bad
		for _, p := range parts[:len(parts)-1] {
			m = m[p].(map[string]any)
		}
		m[parts[len(parts)-1]] = "private-canary"
		if err := codex.CheckFileEditConfig(bad); err == nil || strings.Contains(err.Error(), "private-canary") {
			t.Errorf("unsafe %s was accepted or leaked", path)
		}
	}
	for _, key := range []string{"developer_instructions", "instructions", "model_instructions_file", "compact_prompt"} {
		b, _ := json.Marshal(good)
		var bad map[string]any
		_ = json.Unmarshal(b, &bad)
		bad[key] = "historical-canary"
		if err := codex.CheckFileEditConfig(bad); err == nil {
			t.Errorf("accepted injected %s", key)
		}
	}
	good["mcp_servers"] = map[string]any{"surprise": map[string]any{"command": "private-canary"}}
	if err := codex.CheckFileEditConfig(good); err == nil {
		t.Fatal("enabled MCP accepted")
	}
}

func TestControlledProfileBeforeTurn(t *testing.T) {
	for _, mut := range []string{"valid", "profile", "roots", "instructions", "config", "permission_rules"} {
		t.Run(mut, func(t *testing.T) {
			spec := startSpec(t, "")
			source := filepath.Join(spec.Dir, "AGENTS.md")
			config := codex.FileEditSettings()
			config["permissions"] = map[string]any{"fabric_demo_test": codex.FileEditPermissions(spec.Dir)}
			reply := map[string]any{"thread": map[string]any{"id": "thr_1"}, "cwd": spec.Dir, "approvalPolicy": "never", "activePermissionProfile": map[string]any{"id": "fabric_demo_test"}, "runtimeWorkspaceRoots": []string{spec.Dir}, "instructionSources": []string{source}}
			switch mut {
			case "profile":
				reply["activePermissionProfile"] = map[string]any{"id": "widened"}
			case "roots":
				reply["runtimeWorkspaceRoots"] = []string{"/"}
			case "instructions":
				reply["instructionSources"] = []string{"/private/history"}
			case "config":
				config["features"].(map[string]any)["shell_tool"] = true
			case "permission_rules":
				config["permissions"].(map[string]any)["fabric_demo_test"].(map[string]any)["filesystem"] = map[string]any{":root": "read"}
			}
			cb, _ := json.Marshal(map[string]any{"config": config})
			rb, _ := json.Marshal(reply)
			scriptPath := filepath.Join(t.TempDir(), "controlled.script")
			if err := os.WriteFile(scriptPath, []byte("on initialize reply {}\non config/read reply "+string(cb)+"\non thread/start reply "+string(rb)+"\non turn/start exit 99\n"), 0600); err != nil {
				t.Fatal(err)
			}
			a := &codex.Adapter{Command: []string{exe(t), "--fabric-test-script", scriptPath}, Control: &codex.Control{Profile: "fabric_demo_test", InstructionSources: []string{source}, PreflightOnly: true}, KillGrace: 300 * time.Millisecond, RequestTimeout: time.Second}
			sess, rec := begin(t, a, spec)
			f := finish(t, sess)
			want := harness.OutcomeFailed
			if mut == "valid" {
				want = harness.OutcomeStopped
			}
			if f.Outcome != want {
				t.Fatalf("outcome %+v, want %s", f, want)
			}
			if mut == "valid" && f.Reason != "controlled preflight complete; no model turn" {
				t.Fatalf("valid preflight did not reach checked thread: %+v", f)
			}
			for _, e := range rec.Snapshot() {
				if e.Kind == harness.KindTurn || e.Kind == harness.KindMessage {
					t.Fatal("model input emitted during preflight")
				}
			}
		})
	}
}

func TestControlledModeRejectsAmbientEnvironmentAndResume(t *testing.T) {
	a := adapter(t, "normal")
	a.Control = &codex.Control{Profile: "fabric_demo_test", PreflightOnly: true}
	for _, spec := range []harness.StartSpec{
		{SessionID: "x", Dir: t.TempDir(), Prompt: "x", Env: map[string]string{"OPENAI_API_KEY": "canary"}},
		{SessionID: "x", Dir: t.TempDir(), Prompt: "x", Extra: json.RawMessage(`{"resume_thread":"history"}`)},
	} {
		if s, e := a.Start(context.Background(), spec, &harnesstest.Recorder{}); e == nil {
			_ = s.Stop(context.Background())
			t.Fatal("unsafe input accepted")
		}
	}
}
