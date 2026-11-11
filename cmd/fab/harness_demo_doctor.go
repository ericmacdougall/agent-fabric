package main

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

type privateDoctorCapture struct{ data []byte }

func (c *privateDoctorCapture) Write(b []byte) (int, error) {
	if len(c.data)+len(b) > 64<<10 {
		return 0, errors.New("private metadata limit")
	}
	c.data = append(c.data, b...)
	return len(b), nil
}

var terminalEscape = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)

// checkClaudeDoctor reduces only documented remote-policy status. It does not
// publish lines, causes, paths, account details or arbitrary settings values.
// Unknown wording, loaded/cached policy and every failed/skipped fetch refuse.
func checkClaudeDoctor(raw []byte) error {
	if len(raw) == 0 || len(raw) > 64<<10 {
		return errors.New("claude: managed status unavailable")
	}
	text := terminalEscape.ReplaceAllString(string(raw), "")
	seen := 0
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		const prefix = "Managed settings (remote):"
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		seen++
		value := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(line, prefix)))
		if value != "no server-managed settings configured" && value != "no policy configured" {
			return errors.New("claude: managed status requires review")
		}
	}
	if seen != 1 {
		return errors.New("claude: managed status unavailable or ambiguous")
	}
	return nil
}

// This native metadata command runs only after the caller's proven outer OS
// boundary. Its stdout is private bounded memory, stderr discarded. It is not
// a model turn or a full-config response; main must separately authorize it.
func checkClaudeManagedRuntime(ctx context.Context, profile, settings string, env map[string]string, dir string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	argv := mediatedCommand(profile, []string{claudeDemoBinary, "--restricted", "--settings", settings, "doctor"})
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.WaitDelay = 2 * time.Second
	cmd.Dir = dir
	cmd.Env = []string{}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	capture := &privateDoctorCapture{}
	cmd.Stdout = capture
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return errors.New("claude: native managed-status metadata refused")
	}
	return checkClaudeDoctor(capture.data)
}
