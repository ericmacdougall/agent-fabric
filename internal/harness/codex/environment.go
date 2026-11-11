package codex

import "path/filepath"

// FileEditEnvironment selects the same real native home whose historical roots
// are guarded by the caller's outer profile. It never inherits ambient values.
func FileEditEnvironment(home string) map[string]string {
	return map[string]string{
		"PATH": "/usr/bin:/bin", "HOME": home, "CODEX_HOME": filepath.Join(home, ".codex"),
		"LANG": "C.UTF-8", "LC_ALL": "C", "SHELL": "/bin/sh",
		"PYTHONNOUSERSITE": "1", "PYTHONDONTWRITEBYTECODE": "1", "RUST_LOG": "off",
		"AGENT_HARNESS_TASK_OUTPUT_PRUNE": "off",
	}
}
