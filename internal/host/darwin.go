//go:build darwin

package host

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Darwin runs a worker in its own session with a per-run token in its
// environment, confined by a generated Seatbelt profile through sandbox-exec
// (E5). A process group is not containment on macOS (Kimi N6): stop signals
// the group, then inventories every own-user process carrying the token,
// signals survivors individually after re-checking identity, and verifies
// none remain (DESIGN.md 3.7). The Mac advertises P0 (review) only (D6);
// P1 is accepted for the negative controls and refused for real grants.
type Darwin struct {
	SandboxExec string   // path to sandbox-exec; "" disables Seatbelt (controls then fail, by design)
	ExtraRW     []string // host-wide writable paths (~/.claude, caches)
	AllowP1     bool     // only the negative-control runner sets this

	mu   sync.Mutex
	cmds map[string]*exec.Cmd
}

func (d *Darwin) Name() string { return "darwin-session-token-seatbelt" }

func (d *Darwin) profile(g Grant) string {
	rw := []string{g.RunDir, "/private/tmp", "/private/var/folders", "/dev"}
	if g.Profile == "P1" {
		rw = append(rw, g.Worktree)
	}
	rw = append(rw, d.ExtraRW...)
	var b strings.Builder
	b.WriteString("(version 1)\n(allow default)\n(deny file-write*)\n(allow file-write*")
	for _, p := range rw {
		if p == "" {
			continue
		}
		if real, err := filepath.EvalSymlinks(p); err == nil {
			p = real
		}
		fmt.Fprintf(&b, "\n  (subpath %q)", p)
	}
	b.WriteString(")\n")
	return b.String()
}

// Start launches argv in a new session under sandbox-exec with FAB_RUN set.
func (d *Darwin) Start(g Grant, argv []string, env []string, stdout, stderr *os.File) (Handle, error) {
	if err := g.Validate(); err != nil {
		return Handle{}, err
	}
	if g.Profile == "P1" && !d.AllowP1 {
		return Handle{}, fmt.Errorf("%w: this host runs review (P0) and browser lanes only (D6)", ErrGrantInvalid)
	}
	if d.SandboxExec == "" {
		return Handle{}, errors.New("darwin profile: sandbox-exec not configured; refusing an unconfined launch")
	}
	profilePath := filepath.Join(g.RunDir, "seatbelt-"+string(g.Run)+".sb")
	if err := os.WriteFile(profilePath, []byte(d.profile(g)), 0o600); err != nil {
		return Handle{}, err
	}
	// Second attribution signal: every descendant inherits fd 3 on this file
	// unless it deliberately closes it (platform binaries hide their env).
	runFile := filepath.Join(g.RunDir, ".fab-run-"+string(g.Run))
	rf, err := os.OpenFile(runFile, os.O_RDONLY|os.O_CREATE, 0o600)
	if err != nil {
		return Handle{}, err
	}
	full := append([]string{d.SandboxExec, "-f", profilePath}, argv...)
	cmd := exec.Command(full[0], full[1:]...)
	cmd.Dir = g.Worktree
	cmd.Env = append(append(os.Environ(), "FAB_RUN="+g.Token), env...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.ExtraFiles = []*os.File{rf}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		rf.Close()
		return Handle{}, err
	}
	rf.Close() // the child holds its own copy; ours would only show up in lsof
	d.mu.Lock()
	if d.cmds == nil {
		d.cmds = map[string]*exec.Cmd{}
	}
	d.cmds[string(g.Run)] = cmd
	d.mu.Unlock()
	return Handle{Run: g.Run, Token: g.Token, PID: cmd.Process.Pid, PGID: cmd.Process.Pid, RunFile: runFile}, nil
}

// Wait returns the worker's exit code.
func (d *Darwin) Wait(h Handle) (int, error) {
	d.mu.Lock()
	cmd := d.cmds[string(h.Run)]
	d.mu.Unlock()
	if cmd == nil {
		return -1, errors.New("unknown handle")
	}
	err := cmd.Wait()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	return 0, err
}

// Inventory attributes own-user processes to the run by two inherited
// signals: FAB_RUN=<token> in the environment (kern.procargs2) and an open
// descriptor on the run file (lsof -t). Apple platform binaries such as
// /bin/sh and /bin/sleep hide their environment from other processes, so the
// descriptor is what finds them; a process that is both a platform binary and
// closes inherited descriptors is a known limit, disclosed in the design.
func (d *Darwin) Inventory(h Handle) ([]Process, error) {
	all, err := ownProcesses()
	if err != nil {
		return nil, err
	}
	byFD := map[int]bool{}
	if h.RunFile != "" {
		if out, err := exec.Command("lsof", "-t", "-w", h.RunFile).Output(); err == nil {
			for _, f := range strings.Fields(string(out)) {
				if pid, err := strconv.Atoi(f); err == nil {
					byFD[pid] = true
				}
			}
		}
	}
	var procs []Process
	for _, p := range all {
		if p.PID == os.Getpid() {
			continue // never attribute ourselves
		}
		cmd, byEnv := hasToken(p.PID, h.Token)
		if !byEnv && !byFD[p.PID] {
			continue
		}
		if cmd == "" {
			if argv, _, ok := procArgsEnv(p.PID); ok {
				cmd = strings.Join(argv, " ")
			}
		}
		p.Command = cmd
		procs = append(procs, p)
	}
	return procs, nil
}

func (d *Darwin) same(p Process, h Handle) bool {
	inv, _ := d.Inventory(h)
	for _, q := range inv {
		if q.PID == p.PID && q.Start == p.Start {
			return true
		}
	}
	return false
}

// Stop: signal the group, then reap by token inventory with identity
// re-checked per pid, then verify.
func (d *Darwin) Stop(h Handle, reason string, grace time.Duration) (StopReceipt, error) {
	rc := StopReceipt{Run: h.Run, Reason: reason, StartedAt: time.Now()}
	_ = syscall.Kill(-h.PGID, syscall.SIGTERM)
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if inv, _ := d.Inventory(h); len(inv) == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		inv, _ := d.Inventory(h)
		if len(inv) == 0 {
			break
		}
		for _, p := range inv {
			if d.same(p, h) { // identity re-checked: pid and start time, token still present
				_ = syscall.Kill(p.PID, sig)
				rc.Signalled = append(rc.Signalled, p.PID)
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	d.mu.Lock()
	if cmd := d.cmds[string(h.Run)]; cmd != nil {
		go cmd.Wait() //nolint:errcheck // reap the leader if it is ours
	}
	d.mu.Unlock()
	rc.Survivors, _ = d.Inventory(h)
	rc.Verified = len(rc.Survivors) == 0
	rc.FinishedAt = time.Now()
	return rc, nil
}

// escapeScript double-detaches a sleeper into its own session (Kimi N6).
const escapeScript = `python3 -c 'import os,time
os.setsid()
time.sleep(300)' </dev/null >/dev/null 2>&1 &
sleep 300`

// NegativeControls proves on this Mac that (1) killpg misses a setsid
// grandchild, (2) the token inventory reap gets it, and (3) Seatbelt denies a
// write outside the allowed set while allowing the run dir.
func (d *Darwin) NegativeControls(scratch string) ([]ControlResult, error) {
	var out []ControlResult
	add := func(name string, ok bool, detail string) { out = append(out, ControlResult{name, ok, detail}) }
	d.AllowP1 = true
	g := controlGrant(scratch)
	devnull, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	defer devnull.Close()

	h, err := d.Start(g, []string{"sh", "-c", escapeScript}, nil, devnull, devnull)
	if err != nil {
		return nil, err
	}
	time.Sleep(700 * time.Millisecond)
	before, _ := d.Inventory(h)
	groups := map[int]bool{}
	for _, p := range before {
		groups[p.PGID] = true
	}
	// 1. control: killpg of the leader's group.
	_ = syscall.Kill(-h.PGID, syscall.SIGKILL)
	time.Sleep(400 * time.Millisecond)
	after, _ := d.Inventory(h)
	var escaped []Process
	for _, p := range after {
		if p.PGID != h.PGID {
			escaped = append(escaped, p)
		}
	}
	add("killpg_misses_setsid_grandchild", len(before) >= 2 && len(groups) >= 2 && len(escaped) >= 1,
		fmt.Sprintf("before=%d processes in %d groups; after the group signal=%d survivors outside the group (expected >=1); seen=%s", len(before), len(groups), len(escaped), describe(before)))
	// 2. the token reap gets the survivor.
	rc, _ := d.Stop(h, "negative control", 2*time.Second)
	add("token_inventory_reaps_setsid_grandchild", rc.Verified && len(rc.Signalled) >= 1,
		fmt.Sprintf("signalled=%v survivors=%d", rc.Signalled, len(rc.Survivors)))

	// 3. Seatbelt: outside write denied, run-dir write allowed.
	outside := filepath.Join(os.Getenv("HOME"), fmt.Sprintf(".fab-nc-%d", time.Now().UnixNano()))
	inside := filepath.Join(g.RunDir, "nc-inside.txt")
	h, err = d.Start(g, []string{"sh", "-c", fmt.Sprintf("echo x > %q; echo y > %q", outside, inside)}, nil, devnull, devnull)
	if err != nil {
		return out, err
	}
	d.Wait(h) //nolint:errcheck
	_, errOut := os.Stat(outside)
	_, errIn := os.Stat(inside)
	os.Remove(outside)
	add("seatbelt_denies_write_outside", errors.Is(errOut, os.ErrNotExist) && errIn == nil,
		fmt.Sprintf("outside exists=%v inside exists=%v", errOut == nil, errIn == nil))
	d.AllowP1 = false
	return out, nil
}
