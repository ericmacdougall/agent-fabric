package codex_test

import (
	"context"
	"encoding/json"
	"fmt"
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

func featureMetadata(overrides map[string]bool) []byte {
	var b strings.Builder
	for name, want := range codex.FileEditSettings()["features"].(map[string]any) {
		enabled := want.(bool)
		if name == "unified_exec" {
			enabled = true
		}
		if v, ok := overrides[name]; ok {
			enabled = v
		}
		fmt.Fprintf(&b, "%s stable %t\n", name, enabled)
	}
	return []byte(b.String())
}

func TestFileEditOnlyFeatureClosure(t *testing.T) {
	if err := codex.CheckFileEditFeatures(featureMetadata(nil)); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"shell_tool", "apps", "plugins", "multi_agent", "memories", "code_mode_host", "browser_use"} {
		if err := codex.CheckFileEditFeatures(featureMetadata(map[string]bool{name: true})); err == nil {
			t.Errorf("allowed %s", name)
		}
	}
	if err := codex.CheckFileEditFeatures(featureMetadata(map[string]bool{"hooks": false})); err == nil {
		t.Fatal("allowed disabled hooks")
	}
	if err := codex.CheckFileEditFeatures([]byte("shell_tool stable false\n")); err == nil {
		t.Fatal("accepted incomplete metadata")
	}
}

func TestDisableIDsAreValuesNotDottedOverrides(t *testing.T) {
	argv, err := codex.FileEditCommand("/pinned/codex", "fabric_demo_test", "/owned", codex.DisabledIntegration{Kind: "mcp_servers", ID: `with.dot"quote`})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, "\n")
	if !strings.Contains(joined, `mcp_servers={"with.dot\"quote"={enabled=false}}`) || strings.Contains(joined, "mcp_servers.with.dot") {
		t.Fatal("ID did not stay inside quoted TOML value")
	}
	if !strings.Contains(joined, `"/owned"="read"`) || !strings.Contains(joined, `"/owned/demo.txt"="write"`) || strings.Contains(joined, `.extends=`) {
		t.Fatal("profile failed exact-file boundary")
	}
	if _, err := codex.FileEditCommand("/pinned/codex", "fabric_demo_test", "/owned", codex.DisabledIntegration{Kind: "command", ID: "bad"}); err == nil {
		t.Fatal("accepted unapproved override kind")
	}
}

func TestControlledProfileBeforeTurn(t *testing.T) {
	for _, mut := range []string{"valid", "profile", "roots", "instructions", "unavailable", "paginated", "provider", "model", "catalog"} {
		t.Run(mut, func(t *testing.T) {
			spec := startSpec(t, "")
			source := filepath.Join(spec.Dir, "AGENTS.md")
			profiles := map[string]any{"data": []any{map[string]any{"id": "fabric_demo_test", "allowed": true}}, "nextCursor": nil}
			reply := map[string]any{"thread": map[string]any{"id": "thr_1"}, "cwd": spec.Dir, "approvalPolicy": "never", "modelProvider": "openai", "model": codex.DemoModel, "activePermissionProfile": map[string]any{"id": "fabric_demo_test"}, "runtimeWorkspaceRoots": []string{spec.Dir}, "instructionSources": []string{source}}
			switch mut {
			case "profile":
				reply["activePermissionProfile"] = map[string]any{"id": "widened"}
			case "roots":
				reply["runtimeWorkspaceRoots"] = []string{"/"}
			case "instructions":
				reply["instructionSources"] = []string{"/private/history"}
			case "unavailable":
				profiles["data"] = []any{map[string]any{"id": "fabric_demo_test", "allowed": false}}
			case "paginated":
				profiles["nextCursor"] = "more"
			case "provider":
				reply["modelProvider"] = "other"
			case "model":
				reply["model"] = "unapproved"
			}
			cb, _ := json.Marshal(profiles)
			rb, _ := json.Marshal(reply)
			scriptPath := filepath.Join(t.TempDir(), "controlled.script")
			if err := os.WriteFile(scriptPath, []byte("on initialize reply {}\non config/read exit 98\non permissionProfile/list reply "+string(cb)+"\non thread/start reply "+string(rb)+"\non turn/start exit 99\n"), 0600); err != nil {
				t.Fatal(err)
			}
			a := &codex.Adapter{Command: []string{exe(t), "--fabric-test-script", scriptPath}, Control: &codex.Control{Profile: "fabric_demo_test", InstructionSources: []string{source}, PreflightOnly: true, CatalogOnly: mut == "catalog"}, KillGrace: 300 * time.Millisecond, RequestTimeout: time.Second}
			sess, rec := begin(t, a, spec)
			f := finish(t, sess)
			want := harness.OutcomeFailed
			if mut == "valid" || mut == "catalog" {
				want = harness.OutcomeStopped
			}
			if f.Outcome != want {
				t.Fatalf("outcome %+v, want %s", f, want)
			}
			if mut == "catalog" && f.Reason != "controlled profile metadata checked; no thread or model turn" {
				t.Fatal("catalog check started a thread")
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
	a.Command = []string{exe(t), "--fabric-test-script", script(t, "normal")}
	a.BaseEnv = nil
	a.Control = &codex.Control{Profile: "fabric_demo_test", InstructionSources: []string{"/owned/AGENTS.md"}, PreflightOnly: true}
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
