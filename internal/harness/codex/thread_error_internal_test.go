package codex

import (
	"strings"
	"testing"
)

func TestControlledCoreWrapperShapes(t *testing.T) {
	const canary = "PUBLIC_WRAPPER_PRIVATE_CANARY"
	const fatal = "error creating thread: Fatal error: "
	const session = fatal + "Failed to initialize session: "
	const missing = "Session storage missing at " + canary + ". Create the directory or choose a different Codex home."
	wrap := func(hint, underlying string) string { return fatal + hint + " (underlying error: " + underlying + ")" }
	for _, tc := range []struct{ name, message, family, io string }{
		{"rules", fatal + "failed to load rules: " + canary, "rules", "other"},
		{"session", session + canary, "session_init", "other"},
		{"agents", session + "failed to load AGENTS.md instructions for environment `" + canary + "`: " + canary, "agents", "other"},
		{"agentsMissingDelimiter", session + "failed to load AGENTS.md instructions for environment `" + canary, "other", "other"},
		{"agentsEmptyIdentity", session + "failed to load AGENTS.md instructions for environment ``: " + canary, "other", "other"},
		{"sandbox", "error creating thread: sandbox error: " + canary, "sandbox", "other"},
		{"mappedMissing", wrap(missing, "No such file or directory (os error 2)"), "mapped_session_io", "missing"},
		{"mappedDenied", wrap("Codex cannot access session files at "+canary+" (permission denied). If sessions were created using sudo, fix ownership: sudo chown -R $(whoami) "+canary, canary+": Permission denied (os error 13)"), "mapped_session_io", "permission_denied"},
		{"mappedOperation", wrap(missing, canary+": Operation not permitted (os error 1)"), "mapped_session_io", "operation_not_permitted"},
		{"mappedExists", wrap("Session storage path "+canary+" is blocked by an existing file. Remove or rename it so Codex can create sessions.", canary), "mapped_session_io", "other"},
		{"mappedInvalid", wrap("Session data under "+canary+" looks corrupt or unreadable. Clearing the sessions directory may help (this will remove saved threads).", canary), "mapped_session_io", "other"},
		{"mappedType", wrap("Session storage path "+canary+" has an unexpected type. Ensure it is a directory Codex can use for session files.", canary), "mapped_session_io", "other"},
		{"emptyPath", wrap("Session storage missing at . Create the directory or choose a different Codex home.", canary), "other", "other"},
		{"emptyUnderlying", wrap(missing, ""), "other", "other"},
		{"extraClose", wrap(missing, "Permission denied (os error 13)") + ")", "other", "other"},
		{"missingClose", strings.TrimSuffix(wrap(missing, "Permission denied (os error 13)"), ")"), "other", "other"},
		{"trailingProse", wrap(missing, "Permission denied (os error 13)") + canary, "other", "other"},
		{"trailingInside", wrap(missing, "Permission denied (os error 13) "+canary), "mapped_session_io", "other"},
		{"wrongErrno", wrap(missing, "Permission denied (os error 130)"), "mapped_session_io", "other"},
		{"wrongSignature", wrap(missing, "not Permission denied (os error 13)"), "mapped_session_io", "other"},
		{"lookalikeHint", wrap("Session storage missing atish "+canary, "Permission denied (os error 13)"), "other", "other"},
		{"misleadingOuter", canary + wrap(missing, "Permission denied (os error 13)"), "other", "other"},
		{"lookalikeOuter", "error creating threadish: " + wrap(missing, "Permission denied (os error 13)"), "other", "other"},
		{"closeNotMapped", fatal + canary + ": Permission denied (os error 13))", "fatal", "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := controlledThreadFailure(tc.message)
			if !strings.Contains(result, "source_family="+tc.family) || !strings.Contains(result, "reported_io="+tc.io) {
				t.Fatal("closed source classification mismatch")
			}
			if strings.Contains(result, canary) {
				t.Fatal("private wrapper escaped")
			}
		})
	}
}

func TestControlledCompleteDisplayRegistry(t *testing.T) {
	const outer = "error creating thread: "
	const canary = "PUBLIC_DISPLAY_PRIVATE_CANARY"
	constants := []struct{ body, label string }{
		{"Required managed configuration has not been loaded; authenticate and reload configuration before starting a session", "managed_config_unloaded"},
		{"turn aborted. Something went wrong? Hit `/feedback` to report the issue.", "turn_aborted"},
		{"shared rollout token budget exhausted", "session_budget_exceeded"},
		{"stream disconnected before completion: Incomplete response returned, reason: content_filter", "content_filter"},
		{"Codex ran out of room in the model's context window. Start a new thread or clear earlier history before retrying.", "context_window_exceeded"},
		{"agent thread limit reached", "agent_limit_reached"},
		{"session configured event was not the first event in the stream", "session_configured_not_first_event"},
		{"timeout waiting for child process to exit", "timeout"},
		{"request timed out", "request_timeout"},
		{"spawn failed: child stdout/stderr not captured", "spawn"},
		{"interrupted (Ctrl-C). Something went wrong? Hit `/feedback` to report the issue.", "interrupted"},
		{"Image poisoning", "invalid_image_request"},
		{"Selected model is at capacity. Please try a different model.", "server_overloaded"},
		{"Policy-restricted capacity is unavailable. Please try again later.", "policy_capacity_unavailable"},
		{"Flex capacity unavailable.", "flex_unavailable"},
		{"Quota exceeded. Check your plan and billing details.", "quota_exceeded"},
		{"To use Codex with your ChatGPT plan, upgrade to Plus: https://chatgpt.com/explore/plus.", "usage_not_included"},
		{"We’re currently experiencing high demand, which may cause temporary errors.", "internal_server_error"},
		{"internal error; agent loop died unexpectedly", "internal_agent_died"},
		{"codex-linux-sandbox was required but not provided", "landlock_sandbox_executable_not_provided"},
	}
	prefixes := []struct{ body, label string }{
		{"stream disconnected before completion: ", "stream"},
		{"Incomplete response returned, reason: ", "response_incomplete"},
		{"rate limit exceeded: ", "rate_limit_exceeded"},
		{"no thread with id: ", "thread_not_found"},
		{"duplicate tool: ", "tool_collision"},
		{"Error while reading the server response: ", "response_stream_failed"},
		{"Connection failed: ", "connection_failed"},
		{"exceeded retry limit, last status: ", "retry_limit"},
		{"Missing environment variable: ", "env_var"},
		{"Fatal error: failed to start rollout recording: ", "rollout_recording"},
		{"Fatal error: ", "fatal"},
	}
	check := func(t *testing.T, message, label string) {
		t.Helper()
		result := controlledThreadFailure(message)
		if !strings.HasSuffix(result, "source_family="+label) || strings.Contains(result, canary) {
			t.Fatal("closed Display registry mismatch or private output")
		}
	}
	for _, tc := range constants {
		t.Run(tc.label, func(t *testing.T) {
			check(t, outer+tc.body, tc.label)
			// A transparent IO/string error can spoof the same native Display. The
			// output is only the signature, never proof of the native Rust variant.
			mutant := controlledThreadFailure(outer + tc.body + canary)
			if strings.HasSuffix(mutant, "source_family="+tc.label) || strings.Contains(mutant, canary) {
				t.Fatal("constant accepted private trailing prose")
			}
			check(t, canary+outer+tc.body, "other")
		})
	}
	for _, tc := range prefixes {
		t.Run(tc.label, func(t *testing.T) {
			check(t, outer+tc.body+canary, tc.label)
			check(t, canary+outer+tc.body+canary, "other")
			if tc.label != "fatal" && tc.label != "rollout_recording" {
				check(t, outer+tc.body, "other")
				check(t, outer+strings.TrimSuffix(tc.body, " ")+canary, "other")
			}
		})
	}
	for _, body := range []string{canary, "model unsupported for account " + canary, "unsupported operation: " + canary, "error creating threadish: " + canary} {
		check(t, outer+body, "other")
	}
	check(t, outer+"Fatal error: ", "other")
	check(t, outer+"Fatal error: failed to start rollout recording: ", "fatal")
}
