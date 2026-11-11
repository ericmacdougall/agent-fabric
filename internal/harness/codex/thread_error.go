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
	}
	if !strings.HasPrefix(message, fatal) {
		return "other", message
	}
	hint, underlying, ok := strings.Cut(strings.TrimPrefix(message, fatal), " (underlying error: ")
	if !ok {
		return "other", message
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
