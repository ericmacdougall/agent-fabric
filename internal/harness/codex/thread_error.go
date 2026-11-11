package codex

import "strings"

// controlledThreadFailure reduces private native error text to fixed facts.
// It is used only for a bound controlled thread/start internal error.
func controlledThreadFailure(message string) string {
	category := "other"
	switch {
	case message == "thread startup task stopped before completing":
		category = "startup_task"
	case strings.HasPrefix(message, "error creating thread: "):
		category = "core_create"
	case strings.HasPrefix(message, "failed to set app server client info: "):
		category = "client_info"
	case strings.HasPrefix(message, "failed to read project: "):
		category = "project"
	}
	reportedIO := "other"
	family, ioMessage := controlledCoreSource(message)
	if category != "other" {
		switch {
		case strings.HasSuffix(ioMessage, ": Permission denied (os error 13)"):
			reportedIO = "permission_denied"
		case strings.HasSuffix(ioMessage, ": Operation not permitted (os error 1)"):
			reportedIO = "operation_not_permitted"
		case strings.HasSuffix(ioMessage, ": No such file or directory (os error 2)"):
			reportedIO = "missing"
		}
	}
	return "category=" + category + " reported_io=" + reportedIO + " source_family=" + family
}

// These are reported wrapper signatures, not diagnoses. Native session startup
// maps any nested IO error to a session-storage hint, even for another producer.
func controlledCoreSource(message string) (family, ioMessage string) {
	const fatal = "error creating thread: Fatal error: "
	const session = fatal + "Failed to initialize session: "
	const agents = session + "failed to load AGENTS.md instructions for environment `"
	body, core := strings.CutPrefix(message, "error creating thread: ")
	if !core {
		return "other", message
	}
	switch {
	case strings.HasPrefix(message, fatal+"failed to load rules: "):
		return "rules", message
	case strings.HasPrefix(message, agents):
		identity, detail, ok := strings.Cut(strings.TrimPrefix(message, agents), "`: ")
		if ok && identity != "" && detail != "" {
			return "agents", message
		}
		return "other", message
	case strings.HasPrefix(message, session):
		return "session_init", message
	case strings.HasPrefix(message, "error creating thread: sandbox error: "):
		return "sandbox", message
	case strings.HasPrefix(message, fatal+"failed to start rollout recording: ") && len(message) > len(fatal+"failed to start rollout recording: "):
		return "rollout_recording", message
	}
	if !strings.HasPrefix(message, fatal) {
		return controlledDisplaySignature(body), message
	}
	hint, underlying, ok := strings.Cut(strings.TrimPrefix(message, fatal), " (underlying error: ")
	if !ok {
		if message == fatal {
			return "other", message
		}
		return "fatal", message
	}
	if !strings.HasSuffix(underlying, ")") {
		return "other", ""
	}
	underlying = strings.TrimSuffix(underlying, ")")
	if underlying == "" || !balancedErrorParentheses(underlying) || !knownSessionIOHint(hint) {
		return "other", ""
	}
	// Restore a fixed separator so a bare known IO message and a contextual IO
	// message both match the existing exact terminal signature checks.
	return "mapped_session_io", ": " + underlying
}

// Display is not an injective representation of CodexErrorDetails: free-form
// errors can imitate these strings. Report only the matched public signature,
// never an asserted Rust variant, entitlement, or private suffix.
func controlledDisplaySignature(body string) string {
	switch body {
	case "Required managed configuration has not been loaded; authenticate and reload configuration before starting a session":
		return "managed_config_unloaded"
	case "turn aborted. Something went wrong? Hit `/feedback` to report the issue.":
		return "turn_aborted"
	case "shared rollout token budget exhausted":
		return "session_budget_exceeded"
	case "stream disconnected before completion: Incomplete response returned, reason: content_filter":
		return "content_filter"
	case "Codex ran out of room in the model's context window. Start a new thread or clear earlier history before retrying.":
		return "context_window_exceeded"
	case "agent thread limit reached":
		return "agent_limit_reached"
	case "session configured event was not the first event in the stream":
		return "session_configured_not_first_event"
	case "timeout waiting for child process to exit":
		return "timeout"
	case "request timed out":
		return "request_timeout"
	case "spawn failed: child stdout/stderr not captured":
		return "spawn"
	case "interrupted (Ctrl-C). Something went wrong? Hit `/feedback` to report the issue.":
		return "interrupted"
	case "Image poisoning":
		return "invalid_image_request"
	case "Selected model is at capacity. Please try a different model.":
		return "server_overloaded"
	case "Policy-restricted capacity is unavailable. Please try again later.":
		return "policy_capacity_unavailable"
	case "Flex capacity unavailable.":
		return "flex_unavailable"
	case "Quota exceeded. Check your plan and billing details.":
		return "quota_exceeded"
	case "To use Codex with your ChatGPT plan, upgrade to Plus: https://chatgpt.com/explore/plus.":
		return "usage_not_included"
	case "We’re currently experiencing high demand, which may cause temporary errors.":
		return "internal_server_error"
	case "internal error; agent loop died unexpectedly":
		return "internal_agent_died"
	case "codex-linux-sandbox was required but not provided":
		return "landlock_sandbox_executable_not_provided"
	}
	for _, shape := range []struct{ prefix, label string }{
		{"stream disconnected before completion: ", "stream"},
		{"Incomplete response returned, reason: ", "response_incomplete"},
		{"rate limit exceeded: ", "rate_limit_exceeded"},
		{"no thread with id: ", "thread_not_found"},
		{"duplicate tool: ", "tool_collision"},
		{"Error while reading the server response: ", "response_stream_failed"},
		{"Connection failed: ", "connection_failed"},
		{"exceeded retry limit, last status: ", "retry_limit"},
		{"Missing environment variable: ", "env_var"},
	} {
		if strings.HasPrefix(body, shape.prefix) && len(body) > len(shape.prefix) {
			return shape.label
		}
	}
	return "other"
}

func knownSessionIOHint(hint string) bool {
	const denied = "Codex cannot access session files at "
	if strings.HasPrefix(hint, denied) {
		path, home, ok := strings.Cut(strings.TrimPrefix(hint, denied), " (permission denied). If sessions were created using sudo, fix ownership: sudo chown -R $(whoami) ")
		return ok && path != "" && home != ""
	}
	for _, shape := range []struct{ prefix, suffix string }{
		{"Session storage missing at ", ". Create the directory or choose a different Codex home."},
		{"Session storage path ", " is blocked by an existing file. Remove or rename it so Codex can create sessions."},
		{"Session data under ", " looks corrupt or unreadable. Clearing the sessions directory may help (this will remove saved threads)."},
		{"Session storage path ", " has an unexpected type. Ensure it is a directory Codex can use for session files."},
	} {
		if strings.HasPrefix(hint, shape.prefix) && strings.HasSuffix(hint, shape.suffix) && len(hint) > len(shape.prefix)+len(shape.suffix) {
			return true
		}
	}
	return false
}

func balancedErrorParentheses(message string) bool {
	depth := 0
	for _, char := range message {
		switch char {
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return depth == 0
}
