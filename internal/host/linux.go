//go:build linux

package host

import (
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

// Linux confines a run in a transient user unit (one cgroup per run,
// measured in E5) and applies Landlock inside the unit through the fab
// binary's __sandbox mode.
//
// What this profile can and cannot prove on a same-UID host (review C1): a
// worker reaches the user manager's bus (Landlock ABI 4 does not govern
// connect(2) to pathname sockets) and can start a sibling unit outside its
// cgroup and outside Landlock, and it can signal same-UID processes. Stop
// therefore (1) never trusts the bus: it reads the cgroup path captured at
// Start, signals the pids in it directly, and verifies by cgroup emptiness;
// (2) also scans /proc for processes carrying the run token outside the
// cgroup (escapees) and signals them; (3) fails closed: any inventory error
// is Verified=false and an error. The NegativeControls report whether the
// bus and signal boundaries hold on this host; until they do, the profile's
// Capabilities say so and the daemon refuses untrusted workers.
type Linux struct {
	Self      string // path to the fab binary that implements `__sandbox`
	MemoryMax string // systemd property values; empty keeps the unit default
	CPUQuota  string
	TasksMax  string
	ReadOnly  []string // extra read roots (toolchains, module caches)
	ExtraRW   []string // extra writable roots; empty by default (review C2)
	TCPPorts  []int    // TCP connect ports allowed to the worker; empty denies all
	// User is the dedicated, locked worker account (D3 as decided by the user
	// on 2026-11-06: fab-worker). When set, units run under that uid through
	// a scoped sudoers rule, so the worker cannot reach the controlling
	// user's bus, files, groups or processes. Empty means same-UID (shadow
	// only; containment is not demonstrated).
	User string

	mu   sync.Mutex
	cmds map[string]*exec.Cmd
}

func (l *Linux) Name() string { return "linux-cgroup-landlock" }

// workerUID resolves the worker account's uid (the daemon's own when User is empty).
func (l *Linux) workerUID() (int, error) {
	if l.User == "" {
		return os.Getuid(), nil
	}
	out, err := exec.Command("id", "-u", l.User).Output()
	if err != nil {
		return 0, fmt.Errorf("worker user %q: %w", l.User, err)
	}
	return strconv.Atoi(strings.TrimSpace(string(out)))
}

// asWorker builds a command that runs tool (systemd-run, systemctl, ...) as
// the worker user against the worker's own user manager.
func (l *Linux) asWorker(tool string, args ...string) *exec.Cmd {
	if l.User == "" {
		return exec.Command(tool, args...)
	}
	uid, _ := l.workerUID()
	full := append([]string{"-n", "-u", l.User, "env",
		fmt.Sprintf("XDG_RUNTIME_DIR=/run/user/%d", uid),
		fmt.Sprintf("DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/%d/bus", uid), tool}, args...)
	return exec.Command("sudo", full...)
}

func unitName(run string) string {
	return "fab-" + strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return '-'
	}, run) + ".service"
}

// Start launches argv under `systemd-run --user --pipe --wait` so the host
// owns the worker's stdio, wraps it in `fab __sandbox`, and resolves the
// unit's cgroup path before returning, so later inventory never needs the bus.
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
	// Explicit environment only (DESIGN 3.7): never the host's.
	for _, e := range append([]string{"FAB_RUN=" + g.Token, "HOME=" + os.Getenv("HOME"), "PATH=" + os.Getenv("PATH"), "LANG=C.UTF-8"}, env...) {
		args = append(args, "-E", e)
	}
	rw := []string{g.RunDir, os.TempDir()}
	if g.Profile == "P1" {
		rw = append(rw, g.Worktree) // review runs cannot write the worktree
	}
	rw = append(rw, l.ExtraRW...)
	sandbox := []string{l.Self, "__sandbox"}
	for _, d := range l.ReadOnly {
		sandbox = append(sandbox, "--ro", d)
	}
	if g.Profile == "P0" {
		sandbox = append(sandbox, "--ro", g.Worktree)
	}
	for _, d := range rw {
		sandbox = append(sandbox, "--rw", d)
	}
	for _, p := range l.TCPPorts {
		sandbox = append(sandbox, "--tcp", strconv.Itoa(p))
	}
	sandbox = append(sandbox, "--")
	args = append(args, "--")
	args = append(args, sandbox...)
	args = append(args, argv...)
	cmd := l.asWorker("systemd-run", args...)
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
	// The unit's cgroup path is deterministic for a user unit, so inventory
	// and stop never ask the bus; a unit that already finished has no
	// directory, which the inventory reads as empty.
	uid, err := l.workerUID()
	if err != nil {
		return Handle{}, err
	}
	h := Handle{Run: g.Run, Token: g.Token, PID: cmd.Process.Pid, Unit: unit, CGroup: userUnitCGroup(uid, unit)}
	return h, nil
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

// userUnitCGroup is where the user manager places a transient service
// (observed in E5 and on this host: app.slice under user@<uid>.service).
func userUnitCGroup(uid int, unit string) string {
	return fmt.Sprintf("/sys/fs/cgroup/user.slice/user-%d.slice/user@%d.service/app.slice/%s", uid, uid, unit)
}

// StartJob launches a host job in its own unit under the worker account.
func (l *Linux) StartJob(unit string, argv []string, env []string) error {
	args := []string{"--user", "--collect", "--quiet", "--unit", unit, "-p", "KillMode=control-group"}
	home := os.Getenv("HOME")
	if l.User != "" {
		home = "/srv/fabric/home"
	}
	for _, e := range append([]string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}, env...) {
		args = append(args, "-E", e)
	}
	args = append(args, "--")
	args = append(args, argv...)
	out, err := l.asWorker("systemd-run", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	return nil
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

// Inventory lists the pids in the run's cgroup (read from sysfs, never the
// bus) plus any same-UID process carrying the run token outside it, marked
// Escaped. A read error is an error, never "no processes" (review H4).
func (l *Linux) Inventory(h Handle) ([]Process, error) {
	if h.CGroup == "" {
		return nil, errors.New("handle has no cgroup path")
	}
	inside := map[int]bool{}
	var out []Process
	b, err := os.ReadFile(filepath.Join(h.CGroup, "cgroup.procs"))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("cgroup.procs: %w", err)
		}
		// A collected unit has no directory; a missing slice tree is a
		// broken cgroup view and must not read as "no processes" (review H4).
		// app.slice itself disappears when it is empty; the manager's own
		// directory is the stable landmark.
		manager := filepath.Dir(filepath.Dir(h.CGroup))
		if _, serr := os.Stat(manager); serr != nil {
			return nil, fmt.Errorf("cgroup tree unavailable at %s: %w", manager, serr)
		}
	}
	for _, f := range strings.Fields(string(b)) {
		pid, err := strconv.Atoi(f)
		if err != nil {
			continue
		}
		if p, ok := procIdentity(pid); ok {
			inside[pid] = true
			out = append(out, p)
		}
	}
	var escapees []Process
	var scanErr error
	if l.User != "" {
		uid, uerr := l.workerUID()
		if uerr != nil {
			return nil, uerr
		}
		escapees, scanErr = uidProcesses(uid, inside, h.CGroup) // worker-uid processes under the manager but outside the run unit
	} else {
		escapees, scanErr = tokenCarriers(h.Token, inside)
	}
	if scanErr != nil {
		return nil, scanErr
	}
	return append(out, escapees...), nil
}

// uidProcesses lists escapees: processes owned by the worker uid whose cgroup
// is under the worker's user manager but is neither the run unit, a job unit,
// the manager's bus nor its init scope. The sudo/systemd-run waiter chain the
// daemon started lives in the daemon's own slice and is not counted.
func uidProcesses(uid int, inside map[int]bool, runCGroup string) ([]Process, error) {
	managerPrefix := fmt.Sprintf("/user.slice/user-%d.slice/user@%d.service/", uid, uid)
	unitRel := strings.TrimPrefix(runCGroup, "/sys/fs/cgroup")
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var out []Process
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || inside[pid] {
			continue
		}
		st, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
		if err != nil {
			continue
		}
		owner := -1
		for _, line := range strings.Split(string(st), "\n") {
			if strings.HasPrefix(line, "Uid:") {
				fmt.Sscanf(strings.TrimSpace(strings.TrimPrefix(line, "Uid:")), "%d", &owner) //nolint:errcheck
				break
			}
		}
		if owner != uid {
			continue
		}
		cg, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
		if err != nil {
			continue
		}
		path := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(cg)), "0::"))
		if !strings.HasPrefix(path, managerPrefix) || path == unitRel || strings.HasPrefix(path, unitRel+"/") {
			continue // not under the worker manager (our waiter chain), or the run unit itself
		}
		rel := strings.TrimPrefix(path, managerPrefix)
		if rel == "init.scope" || strings.HasPrefix(rel, "session.slice/") || strings.HasPrefix(rel, "app.slice/dbus") || strings.HasPrefix(rel, "app.slice/fab-job-") {
			continue // manager infrastructure (init scope, session slice with the bus) and host-owned jobs
		}
		if p, ok := procIdentity(pid); ok {
			p.Escaped = true
			out = append(out, p)
		}
	}
	return out, nil
}

// tokenCarriers scans /proc for own-UID processes whose environment carries
// FAB_RUN=<token> and that are not in the set given (the cgroup). They are
// escapees: started by the run, now outside its unit.
func tokenCarriers(token string, inside map[int]bool) ([]Process, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	needle := []byte("FAB_RUN=" + token)
	self := os.Getpid()
	var out []Process
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self || inside[pid] {
			continue
		}
		env, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
		if err != nil {
			continue // not ours or gone
		}
		for _, kv := range bytes.Split(env, []byte{0}) {
			if bytes.Equal(kv, needle) {
				if p, ok := procIdentity(pid); ok {
					p.Escaped = true
					out = append(out, p)
				}
				break
			}
		}
	}
	return out, nil
}

func procIdentity(pid int) (Process, bool) {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return Process{}, false
	}
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
	start := fields[19]
	cmdline, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	return Process{PID: pid, PPID: ppid, PGID: pgid, Start: start, Command: strings.TrimSpace(string(bytes.ReplaceAll(cmdline, []byte{0}, []byte{' '})))}, true
}

func (l *Linux) signalAll(procs []Process, sig syscall.Signal, rc *StopReceipt) {
	for _, p := range procs {
		if q, ok := procIdentity(p.PID); !ok || q.Start != p.Start {
			continue // gone, or the pid was reused
		}
		var err error
		if l.User != "" {
			err = exec.Command("sudo", "-n", "-u", l.User, "/usr/bin/kill", "-"+strconv.Itoa(int(sig)), strconv.Itoa(p.PID)).Run()
		} else {
			err = syscall.Kill(p.PID, sig)
		}
		if err == nil {
			rc.Signalled = append(rc.Signalled, p.PID)
		}
		if p.Escaped {
			rc.Escaped = append(rc.Escaped, p)
		}
	}
}

// Stop signals every process attributed to the run directly (TERM, grace,
// KILL), asks systemd to stop the unit as a courtesy, and verifies by
// inventory. It never certifies what it could not observe.
func (l *Linux) Stop(h Handle, reason string, grace time.Duration) (StopReceipt, error) {
	rc := StopReceipt{Run: h.Run, Reason: reason, StartedAt: time.Now()}
	finish := func(err error) (StopReceipt, error) {
		rc.FinishedAt = time.Now()
		return rc, err
	}
	procs, err := l.Inventory(h)
	if err != nil {
		rc.Verified = false
		return finish(fmt.Errorf("stop %s: inventory(first): %w", h.Run, err))
	}
	l.signalAll(procs, syscall.SIGTERM, &rc)
	_ = l.asWorker("systemctl", "--user", "kill", "--signal=SIGTERM", h.Unit).Run() // courtesy; not trusted
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		inv, err := l.Inventory(h)
		if err != nil {
			return finish(fmt.Errorf("stop %s: inventory(grace): %w", h.Run, err))
		}
		if len(inv) == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	inv, err := l.Inventory(h)
	if err != nil {
		return finish(fmt.Errorf("stop %s: inventory(after-term): %w", h.Run, err))
	}
	if len(inv) > 0 {
		l.signalAll(inv, syscall.SIGKILL, &rc)
		time.Sleep(150 * time.Millisecond)
	}
	_ = l.asWorker("systemctl", "--user", "stop", h.Unit).Run()
	rc.Survivors, err = l.Inventory(h)
	if err != nil {
		return finish(fmt.Errorf("stop %s: final inventory: %w", h.Run, err))
	}
	rc.Verified = len(rc.Survivors) == 0
	l.mu.Lock()
	if cmd := l.cmds[h.Unit]; cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM) // the systemd-run waiter, if still attached
	}
	l.mu.Unlock()
	if !rc.Verified {
		return finish(fmt.Errorf("stop %s: %d survivors", h.Run, len(rc.Survivors)))
	}
	return finish(nil)
}

// escapeScript double-detaches a sleeper into its own session: the N6 shape.
const escapeScript = `python3 -c 'import os,time
os.setsid()
time.sleep(300)' </dev/null >/dev/null 2>&1 &
sleep 300`

// runControl runs one shell probe inside the profile and returns its output.
func (l *Linux) runControl(g Grant, script string) (string, int, error) {
	outPath := filepath.Join(g.RunDir, fmt.Sprintf("control-%d.out", time.Now().UnixNano()))
	out, err := os.Create(outPath)
	if err != nil {
		return "", -1, err
	}
	h, err := l.Start(g, []string{"sh", "-c", script}, nil, out, out)
	if err != nil {
		out.Close()
		return "", -1, err
	}
	code, _ := l.Wait(h)
	out.Close()
	b, _ := os.ReadFile(outPath)
	return strings.TrimSpace(string(b)), code, nil
}

// NegativeControls proves (or disproves) the Linux boundaries on this host.
// Controls 1-4 are the process and resource boundaries of days 4-5; 5-9 are
// the ones the 8f83662f review added (secret reads, config writes, TCP,
// the user bus, same-UID signals). The last two are expected to FAIL on a
// same-UID host without a user-namespace or a dedicated worker account; a
// failing control is reported, never hidden.
func (l *Linux) NegativeControls(scratch string) ([]ControlResult, error) {
	var out []ControlResult
	add := func(name string, ok bool, detail string) { out = append(out, ControlResult{name, ok, detail}) }
	g := controlGrant(scratch)
	devnull, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	defer devnull.Close()

	// 1. control: a group signal does not reach a setsid grandchild.
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
	add("group_signal_misses_setsid_grandchild", survived, fmt.Sprintf("escapee pid=%d survived=%v (expected true: a group is not containment)", escapee, survived))

	// 2. the same shape inside a unit is reaped by cgroup emptiness.
	h, err := l.Start(g, []string{"sh", "-c", escapeScript}, nil, devnull, devnull)
	if err != nil {
		return out, err
	}
	time.Sleep(700 * time.Millisecond)
	inv, _ := l.Inventory(h)
	groups := map[int]bool{}
	for _, p := range inv {
		groups[p.PGID] = true
	}
	rc, _ := l.Stop(h, "negative control", 2*time.Second)
	add("cgroup_stop_reaps_setsid_grandchild", len(inv) >= 2 && len(groups) >= 2 && rc.Verified,
		fmt.Sprintf("inventory before stop=%d processes in %d groups; survivors after stop=%d; seen=%s", len(inv), len(groups), len(rc.Survivors), describe(inv)))

	// 3. Landlock: outside write denied, inside write allowed.
	outside := filepath.Join(os.Getenv("HOME"), fmt.Sprintf(".fab-nc-%d", time.Now().UnixNano()))
	inside := filepath.Join(g.Worktree, "nc-inside.txt")
	if _, _, err := l.runControl(g, fmt.Sprintf("echo x > %q; echo y > %q", outside, inside)); err != nil {
		return out, err
	}
	_, errOut := os.Stat(outside)
	_, errIn := os.Stat(inside)
	os.Remove(outside)
	add("landlock_denies_write_outside_worktree", errors.Is(errOut, os.ErrNotExist) && errIn == nil,
		fmt.Sprintf("outside exists=%v inside exists=%v", errOut == nil, errIn == nil))

	// 4. MemoryMax ends a 300 MiB allocation.
	small := &Linux{Self: l.Self, MemoryMax: "64M", ReadOnly: l.ReadOnly}
	h, err = small.Start(g, []string{"python3", "-c", "x = bytearray(300 * 1024 * 1024); print(len(x))"}, nil, devnull, devnull)
	if err != nil {
		return out, err
	}
	code, _ := small.Wait(h)
	add("memorymax_ends_allocation", code != 0, fmt.Sprintf("exit=%d", code))

	// 5. Secret paths are unreadable (review H3): $HOME outside the grant.
	secret := filepath.Join(os.Getenv("HOME"), fmt.Sprintf(".fab-nc-secret-%d", time.Now().UnixNano()))
	os.WriteFile(secret, []byte("not for workers\n"), 0o600) //nolint:errcheck
	defer os.Remove(secret)
	res, _, err := l.runControl(g, fmt.Sprintf("cat %q >/dev/null 2>&1 && echo secret=readable || echo secret=denied; ls \"$HOME/.ssh\" >/dev/null 2>&1 && echo ssh=listable || echo ssh=denied", secret))
	if err != nil {
		return out, err
	}
	add("secret_paths_unreadable", strings.Contains(res, "secret=denied") && strings.Contains(res, "ssh=denied"), res)

	// 6. The user's Claude configuration and caches are unwritable (review C2).
	res, _, err = l.runControl(g, `(echo x >> "$HOME/.claude/settings.json.fab-nc") 2>/dev/null && echo claude=writable || echo claude=denied; (mkdir -p "$HOME/.cache/fab-nc") 2>/dev/null && echo cache=writable || echo cache=denied`)
	if err != nil {
		return out, err
	}
	os.Remove(filepath.Join(os.Getenv("HOME"), ".claude", "settings.json.fab-nc"))
	os.Remove(filepath.Join(os.Getenv("HOME"), ".cache", "fab-nc"))
	add("claude_config_and_caches_unwritable", strings.Contains(res, "claude=denied") && strings.Contains(res, "cache=denied"), res)

	// 7. TCP connect is limited to the allowed ports: loopback port 1 must be
	// refused by Landlock (EACCES), not by the absence of a listener (ECONNREFUSED).
	res, _, err = l.runControl(g, `python3 -c '
import socket, errno
s = socket.socket()
try:
    s.connect(("127.0.0.1", 1)); print("tcp=connected")
except OSError as e:
    print("tcp=" + ("denied" if e.errno in (errno.EACCES, errno.EPERM) else "refused:" + str(e.errno)))
'`)
	if err != nil {
		return out, err
	}
	add("tcp_connect_limited_to_allowed_ports", strings.Contains(res, "tcp=denied"), res)

	// 8. The user manager is unreachable: a worker cannot create a unit.
	sibling := fmt.Sprintf("fab-nc-sibling-%d", time.Now().UnixNano()%1_000_000)
	res, _, err = l.runControl(g, fmt.Sprintf(`XDG_RUNTIME_DIR=/run/user/%d DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/%d/bus systemd-run --user --collect --quiet --unit %s sleep 2 >/dev/null 2>&1 && echo unit=created || echo unit=denied`, os.Getuid(), os.Getuid(), sibling))
	if err != nil {
		return out, err
	}
	_ = exec.Command("systemctl", "--user", "stop", sibling+".service").Run()
	add("controlling_user_manager_unreachable", strings.Contains(res, "unit=denied"), res+" (fails on a same-UID host: the pathname bus is outside Landlock ABI 4; passes under a dedicated worker uid)")

	// 9. A foreign same-UID process cannot be signalled.
	res, _, err = l.runControl(g, fmt.Sprintf(`MGR=$(pgrep -u %d -x systemd | head -1); python3 -c "`, os.Getuid())+`
import os, sys
try:
    os.kill(int(sys.argv[1]), 0); print('signal=allowed')
except PermissionError:
    print('signal=denied')
except ProcessLookupError:
    print('signal=nomanager')
" "$MGR"`)
	if err != nil {
		return out, err
	}
	add("controlling_user_process_unsignallable", strings.Contains(res, "signal=denied"), res+" (fails on a same-UID host: Landlock signal scoping needs ABI 6; passes under a dedicated worker uid)")
	return out, nil
}

// findChildSleeper returns the pid of the python sleeper descended from
// parent (it re-parents after its parent sh returns, so walk /proc).
func findChildSleeper(parent int) int {
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		p, ok := procIdentity(pid)
		if !ok || !strings.HasPrefix(p.Command, "python3 -c ") || !strings.Contains(p.Command, "os.setsid()") {
			continue
		}
		if p.PGID == pid && p.PGID != parent {
			return pid
		}
	}
	return 0
}
