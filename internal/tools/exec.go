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
	// SpillID names the saved full output when it was cut and a spill
	// directory is configured; Total is the size of what was saved.
	SpillID string
	Total   int64
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

// BoundaryFromControlsRequired is BoundaryFromControls plus a list of control
// names that must be present and passing: a host that never ran, say, its
// network-denied control has not shown that boundary, however many others passed.
func BoundaryFromControlsRequired(hostName string, results []host.ControlResult, required []string) Boundary {
	b := BoundaryFromControls(hostName, results)
	seen := map[string]bool{}
	for _, r := range results {
		if r.Passed {
			seen[r.Name] = true
		}
	}
	for _, n := range required {
		if !seen[n] {
			b.Contained = false
			b.FailedWith = append(b.FailedWith, "missing:"+n)
		}
	}
	if len(required) == 0 {
		b.Contained = false
		b.FailedWith = append(b.FailedWith, "no required controls configured")
	}
	return b
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
	// ExitCleanup (FABRIC_TOOL_EXIT_CLEANUP=1 in cmd/fab) stops and verifies
	// the child's descendants after every leader exit, not only on timeout:
	// a leader that backgrounds a process otherwise leaves it running after
	// Run returns. Off, Run behaves exactly as before the flag existed.
	ExitCleanup bool
	// Pin, when set, is the run's recorded worktree (goodqa#15283): every
	// child start first checks it and refuses with ErrWorktreeMoved, so the
	// host never starts a child whose cwd is outside the recorded directory.
	Pin *WorktreePin
	// Spill, when set, receives the full output of a cut result (goodqa#15361
	// N2). It is a runner-only directory outside RunDir.
	Spill *RunnerDir
	n     int
}

// cleanupTries bounds how long a failed cleanup keeps the run's handle.
const cleanupTries = 3

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
	if h.Pin != nil {
		if err := h.Pin.Check(); err != nil {
			return ExecResult{}, err
		}
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
	// A child that floods its output file is stopped long before the disk
	// fills: the file is capped well above what is ever read back.
	limit := floodLimit(spec)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	var waitErr error
wait:
	for {
		select {
		case d := <-done:
			res.ExitCode = d.code
			if d.err != nil {
				waitErr = fmt.Errorf("wait for tool %s: %w", hd.Run, d.err)
			}
			break wait
		case <-timer.C:
			res.TimedOut = true
			break wait
		case <-ctx.Done():
			res.TimedOut = true
			break wait
		case <-tick.C:
			if fi, err := out.Stat(); err == nil && fi.Size() > limit {
				res.TimedOut, res.Cut = true, true
				break wait
			}
		}
	}
	if h.ExitCleanup {
		return h.finishWithCleanup(hd, done, out, spec, res, waitErr)
	}
	if res.TimedOut {
		if _, err := h.Host.Stop(hd, "tool timeout", 2*time.Second); err != nil {
			return res, err
		}
		<-done
	}
	res.Output, res.Cut = readCapped(out, spec.MaxOutput)
	h.spill(out, spec, &res)
	return res, nil
}

// finishWithCleanup is the ExitCleanup ending of Run: every started handle
// gets a bounded, verified Stop before Run returns, independent of the tool
// context. A cleanup that cannot be verified is an error beside the output.
func (h *HostConfiner) finishWithCleanup(hd host.Handle, done <-chan struct {
	code int
	err  error
}, out *os.File, spec ExecSpec, res ExecResult, waitErr error) (ExecResult, error) {
	reason := "leader exited; verify descendant cleanup"
	if res.TimedOut {
		reason = "tool timeout"
	}
	cleanupErr := h.cleanup(hd, reason)
	if res.TimedOut && cleanupErr == nil {
		// After a verified Stop the leader is gone: reap it before reading its
		// output. After a failed Stop, never block on a child that may live.
		<-done
	}
	res.Output, res.Cut = readCapped(out, spec.MaxOutput)
	h.spill(out, spec, &res)
	if cleanupErr != nil {
		return res, errors.Join(waitErr, cleanupErr)
	}
	return res, waitErr
}

// cleanup asks the host to stop hd and accepts only a verified receipt with
// no survivors and no escaped processes, retrying a bounded number of times.
func (h *HostConfiner) cleanup(hd host.Handle, reason string) error {
	var lastErr error
	for try := 1; try <= cleanupTries; try++ {
		rc, err := h.Host.Stop(hd, reason, 2*time.Second)
		switch {
		case err != nil:
			lastErr = fmt.Errorf("cleanup for tool %s failed after %d attempt(s): %w", hd.Run, try, err)
		case !rc.Verified:
			lastErr = fmt.Errorf("cleanup for tool %s was not verified after %d attempt(s)", hd.Run, try)
		case len(rc.Survivors) > 0:
			lastErr = fmt.Errorf("cleanup for tool %s left %d survivors after %d attempt(s)", hd.Run, len(rc.Survivors), try)
		case len(rc.Escaped) > 0:
			lastErr = fmt.Errorf("cleanup for tool %s found %d escaped processes after %d attempt(s)", hd.Run, len(rc.Escaped), try)
		default:
			return nil
		}
		if try < cleanupTries {
			time.Sleep(time.Duration(1<<(try-1)) * 10 * time.Millisecond)
		}
	}
	return lastErr
}

func floodLimit(spec ExecSpec) int64 {
	if spec.MaxOutput <= 0 {
		return 4<<20 + 1<<20
	}
	return int64(spec.MaxOutput)*4 + (1 << 20)
}

// spill copies the output from the descriptor Run already holds, never from a
// path the child could have replaced, into the runner-only directory, up to the
// flood cap. A failure leaves the ordinary truncation notice.
func (h *HostConfiner) spill(out *os.File, spec ExecSpec, res *ExecResult) {
	if !res.Cut || h.Spill == nil {
		return
	}
	f, name, err := h.Spill.Create("")
	if err != nil {
		return
	}
	if _, err := out.Seek(0, io.SeekStart); err != nil {
		f.Close()
		h.Spill.Abort(name)
		return
	}
	n, err := io.Copy(f, io.LimitReader(out, floodLimit(spec)))
	f.Close()
	if err != nil {
		h.Spill.Abort(name)
		return
	}
	h.Spill.Done(name, n)
	res.SpillID, res.Total = name, n
	if fi, err := out.Stat(); err == nil && fi.Size() > n {
		res.Total = fi.Size()
	}
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
	// An allowlist entry must match the whole line, and shell control syntax
	// never passes: a pattern such as "^git status" must not admit
	// "git status; curl x | sh".
	if strings.ContainsAny(in.Command, ";&|`$<>\n\r(){}\\") {
		return "shell refused: shell control characters are not allowed", true
	}
	ok := false
	for _, re := range s.Allow {
		if loc := re.FindStringIndex(in.Command); loc != nil && loc[0] == 0 && loc[1] == len(in.Command) {
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
	switch {
	case res.Cut && res.SpillID != "":
		out += fmt.Sprintf("\n[output truncated at %d bytes of %d; full output saved as spill:%s; read it with read {\"path\":\"spill:%s\",\"offset\":%d}]",
			len(res.Output), res.Total, res.SpillID, res.SpillID, strings.Count(res.Output, "\n")+1)
	case res.Cut:
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
	// Pin, when set, is the run's recorded worktree (goodqa#15283), checked
	// before the root is read; the confined child is checked again by
	// HostConfiner.
	Pin *WorktreePin
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
	if g.Pin != nil {
		if err := g.Pin.Check(); err != nil {
			return "grep refused: " + err.Error(), true
		}
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
	if err := NoSymlinkPath(g.Root, p); err != nil {
		return "grep refused: " + err.Error(), true
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
