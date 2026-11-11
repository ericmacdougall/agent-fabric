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
		{"closeNotMapped", fatal + canary + ": Permission denied (os error 13))", "other", "other"},
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
