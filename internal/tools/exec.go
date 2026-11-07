package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"fabric/internal/agent"
	"fabric/internal/domain"
	"fabric/internal/host"
)

// ExecSpec is one confined child.
type ExecSpec struct {
	Argv      []string
	Timeout   time.Duration
	MaxOutput int
}

// ExecResult is what a child produced. Output is combined stdout+stderr,
// cut at MaxOutput.
type ExecResult struct {
	Output   string
	ExitCode int
	TimedOut bool
	Cut      bool
}

// Confiner runs a child inside the host sandbox profile. Implementations own
// the environment: a child never inherits the runner's, so no provider key,
// token or credential path reaches it.
type Confiner interface {
	Run(ctx context.Context, spec ExecSpec) (ExecResult, error)
}

// Starter is the part of a host (Darwin, Linux, or a fake) a Confiner needs.
type Starter interface {
	Start(g host.Grant, argv []string, env []string, stdout, stderr *os.File) (host.Handle, error)
	Wait(h host.Handle) (int, error)
	Stop(h host.Handle, reason string, grace time.Duration) (host.StopReceipt, error)
}

// Boundary is what a host advertised after its negative controls ran.
type Boundary struct {
	Host       string
	Contained  bool
	FailedWith []string
}

// BoundaryFromControls summarises control results the way the daemon does.
func BoundaryFromControls(hostName string, results []host.ControlResult) Boundary {
	ok, failed := host.Capabilities(results)
	return Boundary{Host: hostName, Contained: ok && len(results) > 0, FailedWith: failed}
}

// ErrUnadvertised refuses a native mission on a host that has not shown its
// boundary (AC-7): confinement that was never demonstrated is not confinement.
var ErrUnadvertised = errors.New("host has not advertised the native tool boundary")

// RequireBoundary fails closed.
func RequireBoundary(b Boundary) error {
	if !b.Contained {
		return fmt.Errorf("%w: %s (failed controls: %v)", ErrUnadvertised, b.Host, b.FailedWith)
	}
	return nil
}

// HostConfiner runs each child as its own host run under a derived grant.
type HostConfiner struct {
	Host  Starter
	Base  host.Grant // the attempt's grant: worktree, run dir, profile, model...
	Scrub []string   // extra environment, nil for none
	n     int
}

var _ Confiner = (*HostConfiner)(nil)

// childGrant derives a distinct run id per child so the host's per-run
// bookkeeping (profile file, unit name) never collides.
func (h *HostConfiner) childGrant() host.Grant {
	h.n++
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s/%d", h.Base.Run, h.n)))
	g := h.Base
	g.Run = domain.AttemptID("r_" + hex.EncodeToString(sum[:10]))
	g.Token = hex.EncodeToString(sum[10:26]) + "-" + g.Token[:min(8, len(g.Token))]
	return g
}

func (h *HostConfiner) Run(ctx context.Context, spec ExecSpec) (ExecResult, error) {
	if len(spec.Argv) == 0 {
		return ExecResult{}, errors.New("no argv")
	}
	out, err := os.CreateTemp(h.Base.RunDir, "tool-out-*")
	if err != nil {
		return ExecResult{}, err
	}
	defer os.Remove(out.Name())
	defer out.Close()
	g := h.childGrant()
	env := append([]string{}, h.Scrub...)
	hd, err := h.Host.Start(g, spec.Argv, env, out, out)
	if err != nil {
		return ExecResult{}, err
	}
	done := make(chan struct {
		code int
		err  error
	}, 1)
	go func() {
		c, e := h.Host.Wait(hd)
		done <- struct {
			code int
			err  error
		}{c, e}
	}()
	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	res := ExecResult{}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case d := <-done:
		res.ExitCode = d.code
	case <-timer.C:
		res.TimedOut = true
	case <-ctx.Done():
		res.TimedOut = true
	}
	if res.TimedOut {
		if _, err := h.Host.Stop(hd, "tool timeout", 2*time.Second); err != nil {
			return res, err
		}
		<-done
	}
	res.Output, res.Cut = readCapped(out, spec.MaxOutput)
	return res, nil
}

func readCapped(f *os.File, max int) (string, bool) {
	if max <= 0 {
		max = 32 << 10
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", false
	}
	b, _ := io.ReadAll(io.LimitReader(f, int64(max)+1))
	if len(b) > max {
		return string(b[:max]), true
	}
	return string(b), false
}

// ---- shell ----

// Shell runs one command line under sh -c in the confined child. A command
// must match an allowlist pattern from the grant; an empty allowlist denies
// everything. Shell is always side-effecting: after a crash it is never
// re-run on the model's behalf.
type Shell struct {
	C         Confiner
	Allow     []*regexp.Regexp
	Timeout   time.Duration
	MaxOutput int
}

var _ agent.Tool = (*Shell)(nil)

func (s *Shell) Name() string        { return "shell" }
func (s *Shell) SideEffecting() bool { return true }
func (s *Shell) Def() json.RawMessage {
	return json.RawMessage(`{"name":"shell","description":"Run one allow-listed shell command in the sandboxed worktree.","input_schema":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}`)
}

func (s *Shell) Run(ctx context.Context, input json.RawMessage) (string, bool) {
	var in struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(input, &in); err != nil || strings.TrimSpace(in.Command) == "" {
		return "shell needs {\"command\": \"...\"}", true
	}
	if strings.ContainsRune(in.Command, 0) || len(in.Command) > 4096 {
		return "shell refused: bad command", true
	}
	ok := false
	for _, re := range s.Allow {
		if re.MatchString(in.Command) {
			ok = true
			break
		}
	}
	if !ok {
		return "shell refused: command is not on this grant's allowlist", true
	}
	res, err := s.C.Run(ctx, ExecSpec{Argv: []string{"/bin/sh", "-c", in.Command}, Timeout: s.Timeout, MaxOutput: s.MaxOutput})
	return render(res, err)
}

func render(res ExecResult, err error) (string, bool) {
	if err != nil {
		return "tool failed to run: " + err.Error(), true
	}
	out := res.Output
	if res.Cut {
		out += "\n[output truncated]"
	}
	if res.TimedOut {
		return out + "\n[timed out and stopped]", true
	}
	if res.ExitCode != 0 {
		return fmt.Sprintf("%s\n[exit status %d]", out, res.ExitCode), true
	}
	return out, false
}

// ---- grep ----

// Grep searches files under the worktree with grep, as a confined child with
// a fixed argv: the pattern is data, never shell syntax.
type Grep struct {
	C         Confiner
	Root      string
	Timeout   time.Duration
	MaxOutput int
}

var _ agent.Tool = (*Grep)(nil)

func (g *Grep) Name() string        { return "grep" }
func (g *Grep) SideEffecting() bool { return false }
func (g *Grep) Def() json.RawMessage {
	return json.RawMessage(`{"name":"grep","description":"Search file contents under the worktree. Fixed-string match unless regex is true.","input_schema":{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string"},"regex":{"type":"boolean"}},"required":["pattern"]}}`)
}

func (g *Grep) Run(ctx context.Context, input json.RawMessage) (string, bool) {
	var in struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
		Regex   bool   `json:"regex"`
	}
	if err := json.Unmarshal(input, &in); err != nil || in.Pattern == "" || len(in.Pattern) > 1024 || strings.ContainsRune(in.Pattern, 0) {
		return "grep needs {\"pattern\": \"...\"}", true
	}
	p := in.Path
	if p == "" {
		p = "."
	}
	if filepath.IsAbs(p) || strings.ContainsRune(p, 0) {
		return "grep refused: path must be relative to the worktree", true
	}
	for _, c := range strings.Split(p, "/") {
		if c == ".." {
			return "grep refused: path contains ..", true
		}
	}
	mode := "-F"
	if in.Regex {
		mode = "-E"
	}
	// -I skips binaries; -r does not follow symlinks (-R would); the pattern
	// goes after -e and the path after --, so neither can be read as an option.
	argv := []string{"grep", "-rnI", "--max-count=50", mode, "-e", in.Pattern, "--", p}
	res, err := g.C.Run(ctx, ExecSpec{Argv: argv, Timeout: g.Timeout, MaxOutput: g.MaxOutput})
	if err == nil && res.ExitCode == 1 && !res.TimedOut {
		return "no matches", false
	}
	return render(res, err)
}
