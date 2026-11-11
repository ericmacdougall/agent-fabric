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
	if category != "other" {
		switch {
		case strings.HasSuffix(message, ": Permission denied (os error 13)"):
			reportedIO = "permission_denied"
		case strings.HasSuffix(message, ": Operation not permitted (os error 1)"):
			reportedIO = "operation_not_permitted"
		case strings.HasSuffix(message, ": No such file or directory (os error 2)"):
			reportedIO = "missing"
		}
	}
	return "category=" + category + " reported_io=" + reportedIO
}
