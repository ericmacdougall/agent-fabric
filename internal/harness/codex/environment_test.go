package codex_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fabric/internal/harness"
	"fabric/internal/harness/codex"
	"fabric/internal/harness/harnesstest"
)

func TestFileEditEnvironmentPinsNativeHomeAndMaintenance(t *testing.T) {
	env := codex.FileEditEnvironment("/public/home")
	if env["HOME"] != "/public/home" || env["CODEX_HOME"] != "/public/home/.codex" || env["AGENT_HARNESS_TASK_OUTPUT_PRUNE"] != "off" || env["RUST_LOG"] != "off" || len(env) != 10 {
		t.Fatal("controlled native environment changed")
	}
	for _, key := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "CODEX_SQLITE_HOME", "HTTP_PROXY", "RUST_LOG_STYLE"} {
		if _, exists := env[key]; exists {
			t.Fatal("ambient authority present")
		}
	}
}

func TestControlledPinnedEnvironment(t *testing.T) {
	for _, key := range []string{"valid", "HOME", "CODEX_HOME", "AGENT_HARNESS_TASK_OUTPUT_PRUNE"} {
		t.Run(key, func(t *testing.T) {
			env := codex.FileEditEnvironment("/home/owned")
			if key != "valid" {
				env[key] = "unapproved"
			}
			script := filepath.Join(t.TempDir(), "init-only.script")
			if err := os.WriteFile(script, []byte("on initialize exit 17\n"), 0600); err != nil {
				t.Fatal(err)
			}
			// The public fake is a Go test process. Bound its scheduler without
			// adding a Go-only variable to the native provider environment.
			a := &codex.Adapter{Command: []string{"/usr/bin/env", "GOMAXPROCS=1", exe(t), "--fabric-test-script", script}, BaseEnv: env, Control: &codex.Control{Profile: "fabric_demo_test", InstructionSources: []string{"/home/owned/.codex/AGENTS.md"}, Home: "/home/owned", PreflightOnly: true, CatalogOnly: true}, RequestTimeout: time.Second, KillGrace: 100 * time.Millisecond}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			s, err := a.Start(ctx, startSpec(t, ""), &harnesstest.Recorder{})
			if key != "valid" {
				if err == nil {
					_ = s.Stop(ctx)
					t.Fatal("unbound environment reached process")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			f, err := s.Wait(ctx)
			if err != nil || f.Outcome != harness.OutcomeFailed || f.ExitCode != 17 {
				t.Fatalf("approved environment did not reach fixed fake %+v %v", f, err)
			}
		})
	}
}
