//go:build linux

package host

import (
	"bufio"
	"bytes"
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

// Linux confines a run in a transient user unit (one cgroup per run, measured
// in E5) and applies Landlock inside the unit through the fab binary's
// __sandbox mode. Stop reaches every descendant through the cgroup: an
// unprivileged process cannot leave its cgroup, so the setsid escape of
// Kimi's N6 does not apply (DESIGN.md 3.7).
type Linux struct {
	Self      string // path to the fab binary that implements `__sandbox`
	MemoryMax string // systemd property values; empty keeps the unit default
	CPUQuota  string
	TasksMax  string
	ExtraRW   []string // host-wide writable paths (caches, ~/.claude)

	mu   sync.Mutex
	cmds map[string]*exec.Cmd
}

func (l *Linux) Name() string { return "linux-cgroup-landlock" }

func unitName(run string) string {
	return "fab-" + strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return '-'
	}, run) + ".service"
}

// Start launches argv under `systemd-run --user --pipe --wait` so the host owns
// the worker's stdio, and wraps it in `fab __sandbox` for Landlock. The
// returned handle names the unit; the systemd-run process is the waiter.
func (l *Linux) Start(g Grant, argv []string, env []string, stdout, stderr *os.File) (Handle, error) {
	if err := g.Validate(); err != nil {
		return Handle{}, err
	}
	if l.Self == "" {
		return Handle{}, errors.New("linux profile: Self (fab binary) not set")
	}
	unit := unitName(string(g.Run))
	args := []string{"--user", "--pipe", "--wait", "--collect", "--quiet", "--unit", unit,
		"--working-directory=" + g.Worktree, "-p", "KillMode=control-group"}
	for _, p := range []struct{ k, v string }{{"MemoryMax", l.MemoryMax}, {"CPUQuota", l.CPUQuota}, {"TasksMax", l.TasksMax}} {
		if p.v != "" {
			args = append(args, "-p", p.k+"="+p.v)
		}
	}
	for _, e := range append([]string{"FAB_RUN=" + g.Token, "HOME=" + os.Getenv("HOME"), "PATH=" + os.Getenv("PATH")}, env...) {
		args = append(args, "-E", e)
	}
	rw := append([]string{g.Worktree, g.RunDir, os.TempDir()}, l.ExtraRW...)
	if g.Profile == "P0" {
		rw = append([]string{g.RunDir, os.TempDir()}, l.ExtraRW...) // review runs cannot write the worktree
	}
	sandbox := []string{l.Self, "__sandbox"}
	for _, d := range rw {
		sandbox = append(sandbox, "--rw", d)
	}
	sandbox = append(sandbox, "--")
	args = append(args, "--")
	args = append(args, sandbox...)
	args = append(args, argv...)
	cmd := exec.Command("systemd-run", args...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		return Handle{}, fmt.Errorf("systemd-run: %w", err)
	}
	l.mu.Lock()
	if l.cmds == nil {
		l.cmds = map[string]*exec.Cmd{}
	}
	l.cmds[unit] = cmd
	l.mu.Unlock()
	// The unit may take a moment to appear; the handle is valid regardless.
	return Handle{Run: g.Run, Token: g.Token, PID: cmd.Process.Pid, Unit: unit}, nil
}

// Wait blocks for the systemd-run waiter and returns the worker's exit code.
func (l *Linux) Wait(h Handle) (int, error) {
	l.mu.Lock()
	cmd := l.cmds[h.Unit]
	l.mu.Unlock()
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

func cgroupPath(unit string) (string, error) {
	out, err := exec.Command("systemctl", "--user", "show", "-p", "ControlGroup", "--value", unit).Output()
	if err != nil {
		return "", err
	}
	cg := strings.TrimSpace(string(out))
	if cg == "" {
		return "", nil
	}
	return filepath.Join("/sys/fs/cgroup", cg), nil
}

// Inventory reads the unit's cgroup.procs and attributes each pid by its
// /proc identity (pid, ppid, pgid, start time, cmdline).
func (l *Linux) Inventory(h Handle) ([]Process, error) {
	cg, err := cgroupPath(h.Unit)
	if err != nil || cg == "" {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(cg, "cgroup.procs"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []Process
	for _, f := range strings.Fields(string(b)) {
		pid, err := strconv.Atoi(f)
		if err != nil {
			continue
		}
		if p, ok := procIdentity(pid); ok {
			out = append(out, p)
		}
	}
	return out, nil
}

func procIdentity(pid int) (Process, bool) {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return Process{}, false
	}
	// comm may contain spaces; fields start after the last ')'.
	s := string(stat)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return Process{}, false
	}
	fields := strings.Fields(s[i+1:])
	if len(fields) < 20 {
		return Process{}, false
	}
	ppid, _ := strconv.Atoi(fields[1])
	pgid, _ := strconv.Atoi(fields[2])
	start := fields[19] // starttime in clock ticks since boot: stable identity with the pid
	cmdline, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	return Process{PID: pid, PPID: ppid, PGID: pgid, Start: start, Command: strings.TrimSpace(string(bytes.ReplaceAll(cmdline, []byte{0}, []byte{' '})))}, true
}

// Stop kills the unit's whole cgroup (TERM, grace, KILL), then verifies the
// cgroup is empty. A survivor is reported, never hidden.
func (l *Linux) Stop(h Handle, reason string, grace time.Duration) (StopReceipt, error) {
	rc := StopReceipt{Run: h.Run, Reason: reason, StartedAt: time.Now()}
	before, _ := l.Inventory(h)
	for _, p := range before {
		rc.Signalled = append(rc.Signalled, p.PID)
	}
	_ = exec.Command("systemctl", "--user", "kill", "--signal=SIGTERM", h.Unit).Run()
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if inv, _ := l.Inventory(h); len(inv) == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if inv, _ := l.Inventory(h); len(inv) > 0 {
		_ = exec.Command("systemctl", "--user", "kill", "--signal=SIGKILL", h.Unit).Run()
		time.Sleep(100 * time.Millisecond)
	}
	_ = exec.Command("systemctl", "--user", "stop", h.Unit).Run()
	rc.Survivors, _ = l.Inventory(h)
	rc.Verified = len(rc.Survivors) == 0
	rc.FinishedAt = time.Now()
	l.mu.Lock()
	if cmd := l.cmds[h.Unit]; cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM) // the systemd-run waiter, if still attached
	}
	l.mu.Unlock()
	return rc, nil
}

// escapeScript double-detaches a sleeper into its own session: the N6 shape.
const escapeScript = `python3 -c 'import os,time
os.setsid()
time.sleep(300)' </dev/null >/dev/null 2>&1 &
sleep 300`

// NegativeControls proves the Linux boundaries on this host:
//  1. a setsid grandchild survives killpg of the worker's group (the control
//     that shows a process group is not containment), and
//  2. the same shape inside a unit is reaped by Stop, with an empty cgroup after;
//  3. a write outside the worktree is denied by Landlock and a write inside is allowed;
//  4. a child that blows MemoryMax is killed.
func (l *Linux) NegativeControls(scratch string) ([]ControlResult, error) {
	var out []ControlResult
	add := func(name string, ok bool, detail string) { out = append(out, ControlResult{name, ok, detail}) }

	// 1. control: killpg does not reach a setsid grandchild.
	ctl := exec.Command("sh", "-c", escapeScript)
	ctl.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := ctl.Start(); err != nil {
		return nil, err
	}
	time.Sleep(400 * time.Millisecond)
	escapee := findChildSleeper(ctl.Process.Pid)
	_ = syscall.Kill(-ctl.Process.Pid, syscall.SIGKILL)
	_, _ = ctl.Process.Wait()
	time.Sleep(200 * time.Millisecond)
	survived := escapee > 0 && syscall.Kill(escapee, 0) == nil
	if survived {
		_ = syscall.Kill(escapee, syscall.SIGKILL) // ours: started by this control
	}
	add("killpg_misses_setsid_grandchild", survived, fmt.Sprintf("escapee pid=%d survived=%v (expected true: a group is not containment)", escapee, survived))

	// 2. the same shape inside a unit is reaped.
	g := controlGrant(scratch)
	devnull, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	defer devnull.Close()
	h, err := l.Start(g, []string{"sh", "-c", escapeScript}, nil, devnull, devnull)
	if err != nil {
		return out, err
	}
	time.Sleep(700 * time.Millisecond)
	inv, _ := l.Inventory(h)
	sessions := map[int]bool{}
	for _, p := range inv {
		sessions[p.PGID] = true
	}
	rc, _ := l.Stop(h, "negative control", 2*time.Second)
	add("cgroup_stop_reaps_setsid_grandchild", len(inv) >= 2 && len(sessions) >= 2 && rc.Verified,
		fmt.Sprintf("inventory before stop=%d processes in %d groups; survivors after stop=%d; seen=%s", len(inv), len(sessions), len(rc.Survivors), describe(inv)))

	// 3. Landlock: outside write denied, inside write allowed.
	outside := filepath.Join(os.Getenv("HOME"), fmt.Sprintf(".fab-nc-%d", time.Now().UnixNano()))
	inside := filepath.Join(g.Worktree, "nc-inside.txt")
	h, err = l.Start(g, []string{"sh", "-c", fmt.Sprintf("echo x > %q; echo rc=$? ; echo y > %q; echo rc=$?", outside, inside)}, nil, devnull, devnull)
	if err != nil {
		return out, err
	}
	l.Wait(h) //nolint:errcheck
	_, errOut := os.Stat(outside)
	_, errIn := os.Stat(inside)
	os.Remove(outside)
	add("landlock_denies_write_outside_worktree", errors.Is(errOut, os.ErrNotExist) && errIn == nil,
		fmt.Sprintf("outside exists=%v inside exists=%v", errOut == nil, errIn == nil))

	// 4. MemoryMax kills a 300 MiB allocation.
	small := &Linux{Self: l.Self, MemoryMax: "64M", ExtraRW: l.ExtraRW}
	h, err = small.Start(g, []string{"python3", "-c", "x = bytearray(300 * 1024 * 1024); print(len(x))"}, nil, devnull, devnull)
	if err != nil {
		return out, err
	}
	code, _ := small.Wait(h)
	add("memorymax_kills_allocation", code != 0, fmt.Sprintf("exit=%d", code))
	return out, nil
}

// findChildSleeper returns the pid of the python sleeper descended from pid
// (it has re-parented to init after its parent sh returned, so walk /proc).
func findChildSleeper(parent int) int {
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		p, ok := procIdentity(pid)
		if !ok || !strings.HasPrefix(p.Command, "python3 -c ") || !strings.Contains(p.Command, "os.setsid()") {
			continue // the sh parent carries the same text in its argv; only the python child counts
		}
		// After setsid its group is its own pid, so it is outside the control's group.
		if p.PGID == pid && p.PGID != parent {
			return pid
		}
	}
	return 0
}

// readLines is a small helper for tests.
func readLines(r *os.File) []string {
	var out []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	return out
}
