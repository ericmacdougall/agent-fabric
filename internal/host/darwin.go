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
// environment and an inherited descriptor on a run file, confined by a
// generated Seatbelt profile through sandbox-exec (E5). A process group is
// not containment on macOS (Kimi N6): stop signals the group, then
// inventories by both signals, signals survivors individually after
// re-checking identity, and verifies none remain. The Mac advertises P0
// (review) only (D6); P1 is accepted for the negative controls and refused
// for real grants. The environment is built from an allow-list, never
// inherited (review H3), and the user's secret paths are deny-read.
type Darwin struct {
	SandboxExec string   // path to sandbox-exec; "" disables Seatbelt (controls then fail, by design)
	ExtraRW     []string // extra writable paths; empty by default (review C2)
	AllowP1     bool     // only the negative-control runner sets this

	mu   sync.Mutex
	cmds map[string]*exec.Cmd
}

func (d *Darwin) Name() string { return "darwin-session-token-seatbelt" }

// denyRead lists what no worker may read (DESIGN 3.7 P1; review H3).
func denyRead(home string) []string {
	return []string{
		filepath.Join(home, ".ssh"), filepath.Join(home, ".config", "rclone"), filepath.Join(home, ".claude.json"),
		filepath.Join(home, ".claude"), filepath.Join(home, ".aws"), filepath.Join(home, ".gnupg"),
		filepath.Join(home, "Library", "Keychains"), filepath.Join(home, ".config", "goodqa"), filepath.Join(home, ".netrc"),
	}
}

func (d *Darwin) profile(g Grant) string {
	home := os.Getenv("HOME")
	rw := []string{g.RunDir, "/private/tmp", "/private/var/folders", "/dev"}
	if g.Profile == "P1" {
		rw = append(rw, g.Worktree)
	}
	rw = append(rw, d.ExtraRW...)
	real := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return r
		}
		return p
	}
	var b strings.Builder
	b.WriteString("(version 1)\n(allow default)\n(deny file-write*)\n(allow file-write*")
	for _, p := range rw {
		if p != "" {
			fmt.Fprintf(&b, "\n  (subpath %q)", real(p))
		}
	}
	b.WriteString(")\n(deny file-read*")
	for _, p := range denyRead(home) {
		fmt.Fprintf(&b, "\n  (subpath %q)\n  (literal %q)", real(p), real(p))
	}
	b.WriteString(")\n")
	return b.String()
}

// Start launches argv in a new session under sandbox-exec with an
// allow-listed environment and FAB_RUN set.
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
	runFile := filepath.Join(g.RunDir, ".fab-run-"+string(g.Run))
	rf, err := os.OpenFile(runFile, os.O_RDONLY|os.O_CREATE, 0o600)
	if err != nil {
		return Handle{}, err
	}
	full := append([]string{d.SandboxExec, "-f", profilePath}, argv...)
	cmd := exec.Command(full[0], full[1:]...)
	cmd.Dir = g.Worktree
	if cmd.Env, err = darwinEnv(g, env); err != nil {
		rf.Close()
		return Handle{}, err
	}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.ExtraFiles = []*os.File{rf}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		rf.Close()
		return Handle{}, err
	}
	rf.Close()
	d.mu.Lock()
	if d.cmds == nil {
		d.cmds = map[string]*exec.Cmd{}
	}
	d.cmds[string(g.Run)] = cmd
	d.mu.Unlock()
	return Handle{Run: g.Run, Token: g.Token, PID: cmd.Process.Pid, PGID: cmd.Process.Pid, RunFile: runFile}, nil
}

// StartJob runs a host job as a detached process (no units on macOS).
func (d *Darwin) StartJob(unit string, argv []string, env []string) error {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}, env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait() //nolint:errcheck
	return nil
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
// descriptor on the run file (lsof -t). Apple platform binaries hide their
// environment from other processes, so the descriptor is what finds them. A
// failure of either source is an error, never "no processes" (review H4).
func (d *Darwin) Inventory(h Handle) ([]Process, error) {
	all, err := ownProcesses()
	if err != nil {
		return nil, fmt.Errorf("kern.proc.uid: %w", err)
	}
	byFD := map[int]bool{}
	if h.RunFile == "" {
		return nil, errors.New("handle has no run file")
	}
	out, err := exec.Command("lsof", "-t", "-w", h.RunFile).Output()
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 1 { // exit 1 = no process holds the file
			return nil, fmt.Errorf("lsof: %w", err)
		}
	}
	for _, f := range strings.Fields(string(out)) {
		if pid, err := strconv.Atoi(f); err == nil {
			byFD[pid] = true
		}
	}
	var procs []Process
	for _, p := range all {
		if p.PID == os.Getpid() {
			continue
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
		p.Escaped = p.PGID != h.PGID
		procs = append(procs, p)
	}
	return procs, nil
}

func (d *Darwin) same(p Process, h Handle) bool {
	inv, err := d.Inventory(h)
	if err != nil {
		return false
	}
	for _, q := range inv {
		if q.PID == p.PID && q.Start == p.Start {
			return true
		}
	}
	return false
}

// Stop: signal the group, then reap by inventory with identity re-checked
// per pid, then verify. Any inventory failure is Verified=false and an error.
func (d *Darwin) Stop(h Handle, reason string, grace time.Duration) (StopReceipt, error) {
	rc := StopReceipt{Run: h.Run, Reason: reason, StartedAt: time.Now()}
	finish := func(err error) (StopReceipt, error) {
		rc.FinishedAt = time.Now()
		return rc, err
	}
	_ = syscall.Kill(-h.PGID, syscall.SIGTERM)
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		inv, err := d.Inventory(h)
		if err != nil {
			return finish(fmt.Errorf("stop %s: inventory: %w", h.Run, err))
		}
		if len(inv) == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		inv, err := d.Inventory(h)
		if err != nil {
			return finish(fmt.Errorf("stop %s: inventory: %w", h.Run, err))
		}
		if len(inv) == 0 {
			break
		}
		for _, p := range inv {
			if d.same(p, h) {
				_ = syscall.Kill(p.PID, sig)
				rc.Signalled = append(rc.Signalled, p.PID)
				if p.Escaped {
					rc.Escaped = append(rc.Escaped, p)
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	d.mu.Lock()
	if cmd := d.cmds[string(h.Run)]; cmd != nil {
		go cmd.Wait() //nolint:errcheck
	}
	d.mu.Unlock()
	var err error
	rc.Survivors, err = d.Inventory(h)
	if err != nil {
		return finish(fmt.Errorf("stop %s: final inventory: %w", h.Run, err))
	}
	rc.Verified = len(rc.Survivors) == 0
	if !rc.Verified {
		return finish(fmt.Errorf("stop %s: %d survivors", h.Run, len(rc.Survivors)))
	}
	return finish(nil)
}

// escapeScript double-detaches a sleeper into its own session (Kimi N6).
const escapeScript = `python3 -c 'import os,time
os.setsid()
time.sleep(300)' </dev/null >/dev/null 2>&1 &
sleep 300`

func (d *Darwin) runControl(g Grant, script string) (string, error) {
	outPath := filepath.Join(g.RunDir, fmt.Sprintf("control-%d.out", time.Now().UnixNano()))
	out, err := os.Create(outPath)
	if err != nil {
		return "", err
	}
	h, err := d.Start(g, []string{"sh", "-c", script}, nil, out, out)
	if err != nil {
		out.Close()
		return "", err
	}
	d.Wait(h) //nolint:errcheck
	out.Close()
	b, _ := os.ReadFile(outPath)
	return strings.TrimSpace(string(b)), nil
}

// NegativeControls proves on this Mac that (1) a group signal misses a
// setsid grandchild, (2) the inventory reap gets it, (3) Seatbelt denies a
// write outside the allowed set, (4) secret paths are unreadable, (5) the
// user's Claude configuration is unwritable, (6) the environment is the
// allow-list, not the host's.
func (d *Darwin) NegativeControls(scratch string) ([]ControlResult, error) {
	var out []ControlResult
	add := func(name string, ok bool, detail string) { out = append(out, ControlResult{name, ok, detail}) }
	d.AllowP1 = true
	defer func() { d.AllowP1 = false }()
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
	_ = syscall.Kill(-h.PGID, syscall.SIGKILL)
	time.Sleep(400 * time.Millisecond)
	after, _ := d.Inventory(h)
	var escaped []Process
	for _, p := range after {
		if p.PGID != h.PGID {
			escaped = append(escaped, p)
		}
	}
	add("group_signal_misses_setsid_grandchild", len(before) >= 2 && len(groups) >= 2 && len(escaped) >= 1,
		fmt.Sprintf("before=%d processes in %d groups; after the group signal=%d survivors outside the group (expected >=1); seen=%s", len(before), len(groups), len(escaped), describe(before)))
	rc, _ := d.Stop(h, "negative control", 2*time.Second)
	add("inventory_reaps_setsid_grandchild", rc.Verified && len(rc.Signalled) >= 1,
		fmt.Sprintf("signalled=%v escaped=%d survivors=%d", rc.Signalled, len(rc.Escaped), len(rc.Survivors)))

	outside := filepath.Join(os.Getenv("HOME"), fmt.Sprintf(".fab-nc-%d", time.Now().UnixNano()))
	inside := filepath.Join(g.RunDir, "nc-inside.txt")
	if _, err := d.runControl(g, fmt.Sprintf("echo x > %q; echo y > %q", outside, inside)); err != nil {
		return out, err
	}
	_, errOut := os.Stat(outside)
	_, errIn := os.Stat(inside)
	os.Remove(outside)
	add("seatbelt_denies_write_outside", errors.Is(errOut, os.ErrNotExist) && errIn == nil, fmt.Sprintf("outside exists=%v inside exists=%v", errOut == nil, errIn == nil))

	res, err := d.runControl(g, `ls "$HOME/.ssh" >/dev/null 2>&1 && echo ssh=listable || echo ssh=denied; cat "$HOME/.claude.json" >/dev/null 2>&1 && echo claudejson=readable || echo claudejson=denied`)
	if err != nil {
		return out, err
	}
	add("secret_paths_unreadable", strings.Contains(res, "ssh=denied") && strings.Contains(res, "claudejson=denied"), res)

	res, err = d.runControl(g, `(echo x >> "$HOME/.claude/settings.json.fab-nc") 2>/dev/null && echo claude=writable || echo claude=denied; (mkdir -p "$HOME/Library/Caches/fab-nc") 2>/dev/null && echo cache=writable || echo cache=denied`)
	if err != nil {
		return out, err
	}
	os.Remove(filepath.Join(os.Getenv("HOME"), ".claude", "settings.json.fab-nc"))
	os.Remove(filepath.Join(os.Getenv("HOME"), "Library", "Caches", "fab-nc"))
	add("claude_config_and_caches_unwritable", strings.Contains(res, "claude=denied") && strings.Contains(res, "cache=denied"), res)

	os.Setenv("FAB_NC_LEAK", "host-secret")
	res, err = d.runControl(g, `env | grep -c '^FAB_NC_LEAK=' || true`)
	os.Unsetenv("FAB_NC_LEAK")
	if err != nil {
		return out, err
	}
	add("environment_is_allow_listed", strings.TrimSpace(res) == "0", "host variables seen by the worker: "+res)
	return out, nil
}

// darwinEnv is a Darwin worker's exact environment (goodqa#15087 AC-6):
// fabric's pairs only, a later pair replacing an earlier one. exec.Cmd with
// a non-nil Env passes nothing else.
func darwinEnv(g Grant, env []string) ([]string, error) {
	pairs, _, err := WorkerEnv(append([]string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "USER=" + os.Getenv("USER"),
		"TMPDIR=" + os.TempDir(), "LANG=en_US.UTF-8", "SHELL=/bin/sh", "FAB_RUN=" + g.Token,
	}, env...))
	return pairs, err
}
