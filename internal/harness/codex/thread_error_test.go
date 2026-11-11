package codex_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fabric/internal/harness"
	"fabric/internal/harness/codex"
	"fabric/internal/harness/harnesstest"
)

const threadErrorCanary = "PUBLIC_THREAD_PRIVATE_CANARY"

func TestControlledThreadErrorFacts(t *testing.T) {
	for _, tc := range []struct {
		name, message, category, io string
		code                        int
		method                      string
	}{
		{"startup", "thread startup task stopped before completing", "startup_task", "other", -32603, "thread/start"},
		{"core", "error creating thread: " + threadErrorCanary, "core_create", "other", -32603, "thread/start"},
		{"client", "failed to set app server client info: " + threadErrorCanary, "client_info", "other", -32603, "thread/start"},
		{"project", "failed to read project: " + threadErrorCanary, "project", "other", -32603, "thread/start"},
		{"denied", "error creating thread: " + threadErrorCanary + ": Permission denied (os error 13)", "core_create", "permission_denied", -32603, "thread/start"},
		{"operation", "error creating thread: " + threadErrorCanary + ": Operation not permitted (os error 1)", "core_create", "operation_not_permitted", -32603, "thread/start"},
		{"missing", "error creating thread: " + threadErrorCanary + ": No such file or directory (os error 2)", "core_create", "missing", -32603, "thread/start"},
		{"rules", "error creating thread: Fatal error: failed to load rules: " + threadErrorCanary, "core_create", "other", -32603, "thread/start"},
		{"agents", "error creating thread: Fatal error: Failed to initialize session: failed to load AGENTS.md instructions for environment `" + threadErrorCanary + "`: fs sandbox helper failed with status exit status: 1: " + threadErrorCanary, "core_create", "other", -32603, "thread/start"},
		{"mapped", "error creating thread: Fatal error: Session storage missing at " + threadErrorCanary + ". Create the directory or choose a different Codex home. (underlying error: " + threadErrorCanary + ": No such file or directory (os error 2))", "core_create", "missing", -32603, "thread/start"},
		{"managed", "error creating thread: Required managed configuration has not been loaded; authenticate and reload configuration before starting a session", "core_create", "other", -32603, "thread/start"},
		{"abort", "error creating thread: turn aborted. Something went wrong? Hit `/feedback` to report the issue.", "core_create", "other", -32603, "thread/start"},
		{"event", "error creating thread: session configured event was not the first event in the stream", "core_create", "other", -32603, "thread/start"},
		{"loop", "error creating thread: internal error; agent loop died unexpectedly", "core_create", "other", -32603, "thread/start"},
		{"fatal", "error creating thread: Fatal error: " + threadErrorCanary, "core_create", "other", -32603, "thread/start"},
		{"rollout", "error creating thread: Fatal error: failed to start rollout recording: " + threadErrorCanary, "core_create", "other", -32603, "thread/start"},
		{"provider", "error creating thread: Connection failed: " + threadErrorCanary, "core_create", "other", -32603, "thread/start"},
		{"opaqueModel", "error creating thread: model not available for " + threadErrorCanary, "core_create", "other", -32603, "thread/start"},
		{"unknown", threadErrorCanary, "other", "other", -32603, "thread/start"},
		{"misleadingPrefix", threadErrorCanary + " error creating thread: Permission denied (os error 13)", "other", "other", -32603, "thread/start"},
		{"lookalikePrefix", "error creating threadish: " + threadErrorCanary, "other", "other", -32603, "thread/start"},
		{"startupSuffix", "thread startup task stopped before completing " + threadErrorCanary, "other", "other", -32603, "thread/start"},
		{"signatureSuffix", "error creating thread: Permission denied (os error 13)" + threadErrorCanary, "core_create", "other", -32603, "thread/start"},
		{"wrongErrno", "error creating thread: " + threadErrorCanary + ": Permission denied (os error 130)", "core_create", "other", -32603, "thread/start"},
		{"wrongCode", "error creating thread: " + threadErrorCanary, "", "", -32602, "thread/start"},
		{"knownWrongCode", "error creating thread: Required managed configuration has not been loaded; authenticate and reload configuration before starting a session", "", "", -32602, "thread/start"},
		{"wrongMethod", "error creating thread: " + threadErrorCanary, "", "", -32603, "model/list"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := startSpec(t, codex.DemoPrompt)
			spec.SessionID = "0123456789abcdef0123456789abcdef"
			id := 6 // initialize, requirements, hooks, profiles, models, then thread.
			if tc.method == "model/list" {
				id = 5
			}
			errorFrame, _ := json.Marshal(map[string]any{"id": id, "error": map[string]any{"code": tc.code, "message": tc.message, "data": map[string]string{"private": threadErrorCanary}}})
			hooks := validHookMetadata("/home/owned", spec.Dir)
			script := "on initialize reply {}\non configRequirements/read require-unit-params\non configRequirements/read reply {\"requirements\":{\"allowedLoginMethods\":[\"chatgpt\"]}}\non hooks/list reply " + string(hooks) + "\non permissionProfile/list reply {\"data\":[{\"id\":\"fabric_demo_test\",\"allowed\":true}],\"nextCursor\":null}\n"
			if tc.method == "thread/start" {
				script += "on model/list reply {\"data\":[{\"model\":\"gpt-6-luna\",\"hidden\":false,\"supportedReasoningEfforts\":[{\"reasoningEffort\":\"low\"}]}],\"nextCursor\":null}\non thread/start require-ephemeral\n"
			}
			script += "on " + tc.method + " send " + string(errorFrame) + "\n"
			p := filepath.Join(t.TempDir(), "thread-error.script")
			if err := os.WriteFile(p, []byte(script), 0600); err != nil {
				t.Fatal(err)
			}
			a := &codex.Adapter{Command: []string{"/usr/bin/env", "GOMAXPROCS=1", exe(t), "--fabric-test-script", p}, Control: &codex.Control{Profile: "fabric_demo_test", InstructionSources: []string{"/home/owned/.codex/AGENTS.md"}, Home: "/home/owned"}, RequestTimeout: time.Second, KillGrace: 100 * time.Millisecond}
			rec := &harnesstest.Recorder{}
			var mc bytes.Buffer
			sink, err := codex.NewLifecycleSink(&mc, rec, spec.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			s, err := a.Start(ctx, spec, sink)
			if err != nil {
				t.Fatal(err)
			}
			final, err := s.Wait(ctx)
			if err != nil || final.Outcome != harness.OutcomeFailed {
				t.Fatal("controlled error did not settle")
			}
			if tc.category != "" {
				if !strings.Contains(final.Reason, "category="+tc.category+" reported_io="+tc.io) {
					t.Fatal("fixed thread classification missing")
				}
				family := map[string]string{"rules": "rules", "agents": "agents", "mapped": "mapped_session_io", "managed": "managed_config_unloaded", "abort": "turn_aborted", "event": "session_configured_not_first_event", "loop": "internal_agent_died", "fatal": "fatal", "rollout": "rollout_recording", "provider": "connection_failed"}[tc.name]
				if family == "" {
					family = "other"
				}
				if !strings.Contains(final.Reason, "source_family="+family) {
					t.Fatal("fixed source family missing")
				}
			} else if strings.Contains(final.Reason, "category=") {
				t.Fatal("unrelated error classified")
			}
			b, _ := json.Marshal(rec.Snapshot())
			if strings.Contains(string(b), threadErrorCanary) || strings.Contains(mc.String(), threadErrorCanary) {
				t.Fatal("private error escaped")
			}
		})
	}
}

func TestGenericThreadErrorBehaviorPreserved(t *testing.T) {
	spec := startSpec(t, "public-task")
	p := filepath.Join(t.TempDir(), "generic-error.script")
	if err := os.WriteFile(p, []byte("on initialize reply {}\non thread/start error "+threadErrorCanary+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	a := &codex.Adapter{Command: []string{"/usr/bin/env", "GOMAXPROCS=1", exe(t), "--fabric-test-script", p}, RequestTimeout: time.Second, KillGrace: 100 * time.Millisecond}
	s, _ := begin(t, a, spec)
	final := finish(t, s)
	if final.Outcome != harness.OutcomeFailed || !strings.Contains(final.Reason, threadErrorCanary) || strings.Contains(final.Reason, "category=") {
		t.Fatal("generic caller behavior changed")
	}
}
