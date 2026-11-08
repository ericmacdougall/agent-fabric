package policy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"fabric/internal/agent"
)

// Oracle asks external PreToolUse hooks (the harness's Python guards) about
// a shell command. Each Hook is an argv that reads the hook JSON on stdin
// and exits 2 (or prints a deny decision) to block. The oracle fails closed:
// a hook that is missing, times out or answers unexpectedly denies the
// call, so an absent oracle can never become a silent allow.
type Oracle struct {
	Hooks   [][]string
	Cwd     string
	Timeout time.Duration
	// Env is passed to hooks in addition to a fixed PATH.
	Env []string
	// Precheck, when set, runs before each hook is executed, on every judged
	// call and so in SelfTest too; an error denies the call without running
	// that hook. The native runner sets it to the worktree pin's check
	// (goodqa#15283), so no hook runs once the recorded worktree has moved.
	Precheck func() error
}

var _ agent.Guard = (*Oracle)(nil)

// Check implements agent.Guard. Only the shell tool is judged here; read and
// grep are confined by construction (see internal/tools).
func (o *Oracle) Check(ctx context.Context, tool string, input json.RawMessage) agent.Decision {
	if tool != "shell" {
		return agent.Decision{Allow: true}
	}
	var in struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(input, &in); err != nil || strings.TrimSpace(in.Command) == "" {
		return agent.Decision{Reason: "shell input needs a non-empty command string"}
	}
	if len(o.Hooks) == 0 {
		return agent.Decision{Reason: "policy oracle not configured (fails closed)"}
	}
	payload, _ := json.Marshal(map[string]any{
		"tool_name": "Bash", "tool_input": map[string]string{"command": in.Command}, "cwd": o.Cwd,
		"hook_event_name": "PreToolUse",
	})
	to := o.Timeout
	if to <= 0 {
		to = 10 * time.Second
	}
	for _, h := range o.Hooks {
		if o.Precheck != nil {
			if err := o.Precheck(); err != nil {
				return agent.Decision{Reason: "policy oracle refused before running a hook: " + err.Error()}
			}
		}
		cctx, cancel := context.WithTimeout(ctx, to)
		cmd := exec.CommandContext(cctx, h[0], h[1:]...)
		cmd.Stdin = bytes.NewReader(payload)
		cmd.Env = append([]string{"PATH=/usr/local/bin:/usr/bin:/bin:/opt/homebrew/bin"}, o.Env...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		cancel()
		if err == nil {
			if denied(stdout.Bytes()) {
				return agent.Decision{Reason: firstLine(stdout.String())}
			}
			continue
		}
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 2 {
			return agent.Decision{Reason: firstLine(stderr.String())}
		}
		return agent.Decision{Reason: fmt.Sprintf("policy oracle %s failed (%v): denied", h[len(h)-1], err)}
	}
	return agent.Decision{Allow: true}
}

// denied fails closed: empty output or an explicit allow passes; any other
// decision (deny, ask, a legacy block, unparseable text) denies, because no
// human is present to answer an ask.
func denied(out []byte) bool {
	out = bytes.TrimSpace(out)
	if len(out) == 0 {
		return false
	}
	var d struct {
		H struct {
			Decision string `json:"permissionDecision"`
		} `json:"hookSpecificOutput"`
		Decision string `json:"decision"`
	}
	if json.Unmarshal(out, &d) != nil {
		return true
	}
	if d.Decision != "" && d.Decision != "approve" && d.Decision != "allow" {
		return true
	}
	return d.H.Decision != "" && d.H.Decision != "allow"
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i > 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300]
	}
	if s == "" {
		s = "blocked by policy"
	}
	return s
}

// probes are commands each of the four always-on refusals must deny. They
// are assembled at run time so this source holds no destructive literal.
func probes() map[string]string {
	return map[string]string{
		"secret_dump": "cat ~/.aws/" + "credentials",
		"log_delete":  "rm -rf ~/.claude/" + "wals",
		"broad_kill":  "pkill -f " + "node",
		"prod_delete": "psql postgres://prod-db.internal/app -c \"" + "DROP " + "TABLE users\"",
	}
}

// SelfTest proves the oracle can actually say no. A hook whose imports are
// missing or whose state is unreadable exits 0 (allow) by design, which from
// here would look like a working guard; so a native mission must pass these
// positive controls before it launches.
func (o *Oracle) SelfTest(ctx context.Context) error {
	for name, cmd := range probes() {
		in, _ := json.Marshal(map[string]string{"command": cmd})
		if d := o.Check(ctx, "shell", in); d.Allow {
			return fmt.Errorf("policy oracle self-test: %s probe was allowed; the guard hooks are not working", name)
		}
	}
	if d := o.Check(ctx, "shell", json.RawMessage(`{"command":"ls"}`)); !d.Allow {
		return fmt.Errorf("policy oracle self-test: benign command denied (%s)", d.Reason)
	}
	return nil
}
