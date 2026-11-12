//go:build linux

package host

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"fabric/internal/egress"
)

// Linux confines a run in a transient user unit (one cgroup per run,
// measured in E5) and applies Landlock inside the unit through the fab
// binary's __sandbox mode.
//
// Manager sockets are made inaccessible by a unit property and a real
// preflight, so a worker cannot ask the manager for a sibling outside its
// cgroup, Landlock filesystem, or TCP rules. Stop never trusts the bus: it
// reads the cgroup path captured at Start, signals pids directly, and verifies
// by cgroup emptiness. It also scans /proc for escapees and fails closed on
// inventory errors. Same-UID signal isolation still requires a dedicated
// worker account or a kernel boundary; NegativeControls must pass before the
// profile's Capabilities permit untrusted workers.
type LinuxManager struct {
	Self      string // path to the fab binary that implements `__sandbox`
	MemoryMax string // systemd property values; empty keeps the unit default
	CPUQuota  string
	TasksMax  string
	ReadOnly  []string // extra read roots (toolchains, module caches)
	ExtraRW   []string // extra writable roots; empty by default (review C2)
	TCPPorts  []int    // legacy loopback-only test ports; forbidden with EgressConfig
	LockPath  string   // optional cross-process worker-manager flock; default /run/user/<controller uid>/fab/locks
	// EgressConfig enables the production per-run provider mediator. Start owns
	// its listener and injects its PolicyDescriptor; Stop closes it before the
	// serialization gate can fail over. Nil is reserved for loopback-only tests.
	EgressConfig *egress.Config
	// User is the dedicated, locked worker account (D3 as decided by the user
	// on 2026-11-06: fab-worker). When set, units run under that uid through
	// a scoped sudoers rule, so the worker cannot reach the controlling
	// user's bus, files, groups or processes. Empty means same-UID (shadow
	// only; containment is not demonstrated).
	User string

	mu                   sync.Mutex
	cleanupMu            sync.Mutex // serializes Stop, mediator close, flock release and gate reopening
	launcher             *linuxLauncher
	mgrEnvFile           string // active run's environment file, removed after Wait or successful launcher reaping
	nextSerial           uint64
	activeSerial         uint64
	active               Handle
	runGate              runGate
	runLock              *os.File
	runLockInfo          os.FileInfo
	mediator             *egress.Mediator
	proxyURL             string
	commandFn            func(context.Context, string, ...string) *exec.Cmd // native controls may delay the manager
	writeEnvFile         func(string, []string, bool) error                 // deterministic creation faults in unit tests; defaults to WriteEnvFile
	removeFile           func(string) error                                 // deterministic cleanup faults in unit tests; defaults to os.Remove
	managerCallTimeout   time.Duration
	unitReadinessTimeout time.Duration
	launcherReapTimeout  time.Duration
}

func (l *LinuxManager) Name() string { return "linux-cgroup-landlock" }

// authenticateHandleLocked requires every durable launch identity field, not
// just the process-local serial. A forged same-serial handle therefore cannot
// close the active mediator or release a different unit's serialization.
func (l *LinuxManager) authenticateHandleLocked(h Handle) bool {
	a := l.active
	return l.activeSerial != 0 && h.UnitSerial == l.activeSerial &&
		h.Run == a.Run && h.Token == a.Token && h.PID == a.PID &&
		h.Unit == a.Unit && h.CGroup == a.CGroup && a.UnitSerial == h.UnitSerial
}

func mgrUnitName(run string) string {
	return "fab-" + strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return '-'
	}, run) + ".service"
}

// startArgs retains the original argument-only helper for existing callers.
// Environment values are returned separately by startArgsEnv and are never
// placed on systemd-run's command line.
func (l *LinuxManager) startArgs(g Grant, unit string, argv, env []string) ([]string, error) {
	args, _, err := l.startArgsEnv(g, unit, mgrEnvFile(g), argv, env)
	return args, err
}

func linuxReadyFile(g Grant, unit string) string {
	return filepath.Join(g.RunDir, "jobs", ".fab-ready-"+strings.TrimSuffix(unit, ".service"))
}

func (l *LinuxManager) startArgsWithPolicy(g Grant, unit string, argv, env []string, policy *egress.PolicyDescriptor) ([]string, error) {
	args, _, err := l.startArgsEnvWithPolicy(g, unit, mgrEnvFile(g), argv, env, policy)
	return args, err
}

// startArgsEnv is the systemd-run argument list of a worker and the pairs to
// write to ef. The environment is exact (goodqa#15087 AC-6) and off the
// command line (goodqa#15177): every allow-listed name is blanked with
// -E NAME=, so no value the worker user's systemd manager holds survives,
// and `fab __sandbox --env-keep NAMES --env-file FILE` execs the worker
// with exactly the pairs in the file. No value appears in any argument.
func (l *LinuxManager) startArgsEnv(g Grant, unit, ef string, argv, env []string) ([]string, []string, error) {
	return l.startArgsEnvWithPolicy(g, unit, ef, argv, env, nil)
}

func (l *LinuxManager) startArgsEnvWithPolicy(g Grant, unit, ef string, argv, env []string, policy *egress.PolicyDescriptor) ([]string, []string, error) {
	uid, err := l.workerUID()
	if err != nil {
		return nil, nil, err
	}
	network, err := linuxNetworkPlan(l.TCPPorts, policy)
	if err != nil {
		return nil, nil, err
	}
	// --expand-environment=no: otherwise the manager substitutes $VAR,
	// ${VAR} and $$ in every argv element (the prompt included) from the
	// unit's full environment before __sandbox filters it (inspection
	// 6355639 F1). A systemd older than 254 rejects the flag, so the start
	// fails closed rather than expanding.
	// InaccessiblePaths blocks the manager control sockets inside this unit
	// only. Without it, a worker could request an unmanaged sibling that
	// inherits neither this unit's Landlock filesystem rules nor its TCP rules.
	args := []string{"--user", "--pipe", "--wait", "--collect", "--quiet", "--expand-environment=no", "--unit", unit,
		"--working-directory=" + g.Worktree, "-p", "KillMode=control-group", "-p", managerInaccessiblePathsProperty(uid), "-p", managerSocketFamilyProperty,
		"-p", workerPrivatePIDsProperty, "-p", workerProtectProcProperty, "-p", workerProcSubsetProperty}
	for _, property := range network.properties {
		args = append(args, "-p", property)
	}
	for _, p := range []struct{ k, v string }{{"MemoryMax", l.MemoryMax}, {"CPUQuota", l.CPUQuota}, {"TasksMax", l.TasksMax}} {
		if p.v != "" {
			args = append(args, "-p", p.k+"="+p.v)
		}
	}
	// Explicit environment only (DESIGN 3.7): never the host's. Caller pairs
	// retain the upstream override behavior for ordinary defaults, but FAB_RUN
	// is appended last and therefore remains host-owned. Proxy pairs come only
	// from the validated per-run descriptor; WorkerEnv rejects caller attempts
	// to inject those names before this trusted set is appended.
	pairs := append([]string{"HOME=" + os.Getenv("HOME"), "PATH=" + os.Getenv("PATH"), "LANG=C.UTF-8"}, env...)
	pairs = append(pairs, "FAB_RUN="+g.Token)
	pairs, names, err := WorkerEnv(pairs)
	if err != nil {
		return nil, nil, err
	}
	pairs = append(pairs, network.proxyEnv...)
	names = append(names, linuxProxyEnvironmentNames[:len(network.proxyEnv)]...)
	sort.Strings(names)
	// Blank every allow-listed name so values held by the manager cannot
	// survive. Actual values, including proxy credentials, travel only in ef.
	for _, n := range WorkerEnvNames {
		args = append(args, "-E", n+"=")
	}
	for _, n := range linuxProxyEnvironmentNames {
		args = append(args, "-E", n+"=")
	}
	rw := []string{g.RunDir, os.TempDir()}
	if g.Profile == "P1" {
		rw = append(rw, g.Worktree) // review runs cannot write the worktree
	}
	rw = append(rw, l.ExtraRW...)
	sandbox := []string{l.Self, "__sandbox", "--env-keep", strings.Join(names, ","), "--env-file", ef,
		"--ready-file", linuxReadyFile(g, unit), "--ready-cgroup", strings.TrimPrefix(mgrUserUnitCGroup(uid, unit), "/sys/fs/cgroup")}
	for _, d := range l.ReadOnly {
		sandbox = append(sandbox, "--ro", d)
	}
	if g.Profile == "P0" {
		sandbox = append(sandbox, "--ro", g.Worktree)
	}
	for _, d := range rw {
		sandbox = append(sandbox, "--rw", d)
	}
	for _, p := range network.tcpPorts {
		sandbox = append(sandbox, "--tcp", strconv.Itoa(p))
	}
	sandbox = append(sandbox, "--")
	args = append(args, "--")
	args = append(args, sandbox...)
	return append(args, argv...), pairs, nil
}

// mgrEnvFile is where a run's environment waits for `fab __sandbox`. The trusted
// sandbox can unlink it through the writable jobs directory before exec; the
// read-only run root prevents a worker from renaming the file's parent.
func mgrEnvFile(g Grant) string {
	return filepath.Join(g.RunDir, "jobs", ".fab-env-"+string(g.Run))
}

// removeEnvFile makes controller-side secret cleanup explicit and retryable.
// The worker normally unlinks the file after reading it. Regardless of the
// removal result, verify absence before allowing serialization to be released.
func (l *LinuxManager) removeEnvFile(path string) error {
	if path == "" {
		return nil
	}
	remove := l.removeFile
	if remove == nil {
		remove = os.Remove
	}
	if err := remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove worker environment file: %w", err)
	}
	_, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("verify worker environment file removal: %w", err)
	default:
		return errors.New("worker environment file remains after removal")
	}
}

// clearEnvFile forgets a path only after cleanup succeeds. A failed removal
// remains registered so an authenticated Stop retry must finish it before any
// serialization lock can be released.
func (l *LinuxManager) clearEnvFile(path string) {
	l.mu.Lock()
	if l.mgrEnvFile == path {
		l.mgrEnvFile = ""
	}
	l.mu.Unlock()
}

// preflightManagerIsolation proves the exact unit property used by Start is
// supported and effective on this manager. The probe is trusted fab code, is
// launched by the daemon through the manager, and contains no worker input.
// It runs after the serialization locks so its transient unit cannot race an
// abandoned-run scan or another daemon's preflight. Any failure refuses Start.
func (l *LinuxManager) preflightManagerIsolation(uid int) error {
	return l.preflightUnit(uid, nil)
}

func (l *LinuxManager) preflightUnit(uid int, policy *egress.PolicyDescriptor) error {
	network, err := linuxNetworkPlan(l.TCPPorts, policy)
	if err != nil {
		return err
	}
	if err := l.checkManagerSupport(); err != nil {
		return err
	}
	unit := fmt.Sprintf("fab-manager-preflight-%d-%d.service", os.Getpid(), time.Now().UnixNano())
	args := []string{
		"--user", "--pipe", "--wait", "--collect", "--quiet", "--expand-environment=no",
		"--unit", unit, "-p", "KillMode=control-group", "-p", managerInaccessiblePathsProperty(uid), "-p", managerSocketFamilyProperty,
		"-p", workerPrivatePIDsProperty, "-p", workerProtectProcProperty, "-p", workerProcSubsetProperty,
	}
	for _, property := range network.properties {
		args = append(args, "-p", property)
	}
	args = append(args, "--", l.Self, "__sandbox")
	if policy == nil {
		args = append(args, "--manager-preflight", strconv.Itoa(os.Getpid()), "--")
	} else {
		args = append(args, "--egress-preflight", policy.ProxyURL, strconv.Itoa(os.Getpid()), "--")
	}
	args = append(args, workerManagerControlSockets(uid)...)
	out, err := l.managerOutput("systemd-run", args...)
	if err != nil {
		if reason := unsupportedPreflightReason(string(out)); reason != "" {
			return refuseUnsupported(reason)
		}
		return fmt.Errorf("linux unit-isolation preflight failed (socket, proc or IP controls unsupported or ineffective): %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Start launches argv under `systemd-run --user --pipe --wait` so the host
// owns the worker's stdio, wraps it in `fab __sandbox`, and resolves the
// unit's cgroup path before returning, so later inventory never needs the bus.
func (l *LinuxManager) Start(g Grant, argv []string, env []string, stdout, stderr *os.File) (Handle, error) {
	return l.StartIO(g, argv, env, nil, stdout, stderr)
}

var _ StdinStarter = (*LinuxManager)(nil)

// StartIO is Start with the worker's stdin connected (goodqa#15373 D1). A
// nil stdin is Start.
func (l *LinuxManager) StartIO(g Grant, argv []string, env []string, stdin, stdout, stderr *os.File) (Handle, error) {
	if err := g.Validate(); err != nil {
		return Handle{}, err
	}
	if l.Self == "" {
		return Handle{}, errors.New("linux profile: Self (fab binary) not set")
	}
	unit := mgrUnitName(string(g.Run))
	ef := mgrEnvFile(g)
	readyFile := linuxReadyFile(g, unit)
	if _, err := os.Lstat(readyFile); err == nil {
		return Handle{}, errors.New("worker readiness marker already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return Handle{}, fmt.Errorf("worker readiness marker is unreadable: %w", err)
	}
	if !l.runGate.tryLock() {
		return Handle{}, fmt.Errorf("%w: linux worker manager already has a run or incomplete cleanup", ErrWorkerManagerBusy)
	}
	defer os.Remove(readyFile) //nolint:errcheck
	started, launched := false, false
	var mediator *egress.Mediator
	defer func() {
		if started || launched {
			return // post-launch failure is handled by Stop, never a blind unlock
		}
		if mediator != nil {
			_ = mediator.Close()
		}
		_ = l.releaseRun() // a release failure leaves the gate closed
	}()
	uid, err := l.workerUID()
	if err != nil {
		return Handle{}, err
	}
	if err := l.lockWorkerManager(uid); err != nil {
		return Handle{}, err
	}
	if err := assertNoAbandonedRuns(uid); err != nil {
		return Handle{}, err
	}
	var policy *egress.PolicyDescriptor
	if l.EgressConfig != nil {
		mediator, err = egress.Start(context.Background(), *l.EgressConfig)
		if err != nil {
			return Handle{}, fmt.Errorf("start per-run egress mediator: %w", err)
		}
		p := mediator.Policy()
		policy = &p
	}
	args, pairs, err := l.startArgsEnvWithPolicy(g, unit, ef, argv, env, policy)
	if err != nil {
		return Handle{}, err
	}
	if err := l.preflightUnit(uid, policy); err != nil {
		return Handle{}, err
	}
	// Register before creating the file: a write/close failure followed by a
	// cleanup failure must retain an authenticated retry identity. PID zero is
	// the prelaunch identity; successful exec replaces it before Start returns.
	l.nextSerial++
	serial := l.nextSerial
	h := Handle{Run: g.Run, Token: g.Token, Unit: unit, UnitSerial: serial, CGroup: mgrUserUnitCGroup(uid, unit)}
	l.mu.Lock()
	l.activeSerial = serial
	l.active = h
	l.launcher = nil
	l.mgrEnvFile = ef
	l.mediator = mediator
	l.proxyURL = ""
	if mediator != nil {
		l.proxyURL = mediator.URL()
	}
	l.mu.Unlock()
	// O_EXCL creation prevents a caller or stale run from supplying the
	// environment. Both write and exec failures use verified cleanup below.
	writeEnv := l.writeEnvFile
	if writeEnv == nil {
		writeEnv = WriteEnvFile
	}
	if err := writeEnv(ef, pairs, l.User != ""); err != nil {
		if cleanupErr := l.removeEnvFile(ef); cleanupErr != nil {
			launched = true // retain the prelaunch registration for authenticated Stop
			return h, errors.Join(err, fmt.Errorf("worker environment cleanup failed; retry Stop with this authenticated handle: %w", cleanupErr))
		}
		l.clearEnvFile(ef)
		return Handle{}, err
	}
	launchContext, cancelLaunch := context.WithCancel(context.Background())
	cmd := l.asWorkerContext(launchContext, "systemd-run", args...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.Stdin = nil
	if stdin != nil {
		cmd.Stdin = stdin // a nil *os.File in the interface would not read as no stdin
	}
	if err := cmd.Start(); err != nil {
		cancelLaunch()
		startErr := fmt.Errorf("systemd-run: %w", err)
		if cleanupErr := l.removeEnvFile(ef); cleanupErr != nil {
			launched = true // retain the prelaunch registration for authenticated Stop
			return h, errors.Join(startErr, fmt.Errorf("worker environment cleanup failed; retry Stop with this authenticated handle: %w", cleanupErr))
		}
		l.clearEnvFile(ef)
		return Handle{}, startErr
	}
	launched = true
	launcher := newLinuxLauncher(cmd, cancelLaunch)
	h.PID = cmd.Process.Pid
	l.mu.Lock()
	l.active = h
	l.launcher = launcher
	l.mu.Unlock()
	if err := l.waitUnitReady(h, readyFile, uid); err != nil {
		if _, stopErr := l.Stop(h, "start readiness failure", 0); stopErr != nil {
			return h, fmt.Errorf("linux start readiness: %w; retry Stop with this authenticated handle: %v", err, stopErr)
		}
		return Handle{}, fmt.Errorf("linux start readiness: %w", err)
	}
	started = true
	return h, nil
}

// Wait returns the single preserved launcher wait status. Stop's cancellation
// also unblocks Wait; no second cmd.Wait call can race or lose a fast exit.
func (l *LinuxManager) Wait(h Handle) (int, error) {
	l.mu.Lock()
	if !l.authenticateHandleLocked(h) {
		l.mu.Unlock()
		return -1, errors.New("unknown or stale handle")
	}
	launcher := l.launcher
	ef := l.mgrEnvFile
	l.mu.Unlock()
	if launcher == nil {
		return -1, errors.New("unknown handle")
	}
	// Leader exit is not verified cleanup: the run gate and cross-process
	// worker-manager flock remain held until Stop verifies and releases them.
	code, err := launcher.result()
	if ef != "" {
		// Normally __sandbox has already removed the file after reading it. If
		// not, surface controller-side failure and leave it registered for Stop.
		if cleanupErr := l.removeEnvFile(ef); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("worker environment cleanup after Wait: %w", cleanupErr))
		} else {
			l.clearEnvFile(ef)
		}
	}
	return code, err
}

// mgrUserUnitCGroup is where the user manager places a transient service
// (observed in E5 and on this host: app.slice under user@<uid>.service).
func mgrUserUnitCGroup(uid int, unit string) string {
	return fmt.Sprintf("/sys/fs/cgroup/user.slice/user-%d.slice/user@%d.service/app.slice/%s", uid, uid, unit)
}

// StartJob is disabled: a fab-job-* name is not a safe inventory identity and
// stale registrations must never exempt attacker-created or reused units.
func (l *LinuxManager) StartJob(string, []string, []string) error {
	return errors.New("linux host jobs are disabled: no exact live-job inventory boundary exists")
}

func mgrCgroupPath(unit string) (string, error) {
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
func (l *LinuxManager) Inventory(h Handle) ([]Process, error) {
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
			return nil, fmt.Errorf("cgroup.procs contains invalid pid %q", f)
		}
		p, exists, err := mgrProcIdentity(pid)
		if err != nil {
			return nil, fmt.Errorf("inventory pid %d: %w", pid, err)
		}
		if exists {
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
		escapees, scanErr = mgrUidProcesses(uid, inside, h.CGroup) // worker-uid processes under the manager but outside the run unit
	} else {
		escapees, scanErr = mgrTokenCarriers(h.Token, inside, os.Getuid())
	}
	if scanErr != nil {
		return nil, scanErr
	}
	return append(out, escapees...), nil
}

func (l *LinuxManager) signalAll(procs []Process, sig syscall.Signal, rc *StopReceipt) error {
	for _, p := range procs {
		q, exists, err := mgrProcIdentity(p.PID)
		if err != nil {
			return fmt.Errorf("recheck pid %d before signal: %w", p.PID, err)
		}
		if !exists || q.Start != p.Start {
			continue // demonstrably gone, or the pid was reused
		}
		if l.User != "" {
			err = l.runAsWorker("/usr/bin/kill", "-"+strconv.Itoa(int(sig)), strconv.Itoa(p.PID))
		} else {
			err = syscall.Kill(p.PID, sig)
		}
		if err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("signal pid %d: %w", p.PID, err)
		}
		if err == nil {
			rc.Signalled = append(rc.Signalled, p.PID)
		}
		if p.Escaped {
			rc.Escaped = append(rc.Escaped, p)
		}
	}
	return nil
}

func (l *LinuxManager) currentProxyURL() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.proxyURL
}

// closeMediator authenticates the complete handle for the same reason Stop
// does: a same-serial forged handle cannot tear down the active provider route.
func (l *LinuxManager) closeMediator(h Handle) {
	l.mu.Lock()
	if !l.authenticateHandleLocked(h) {
		l.mu.Unlock()
		return
	}
	mediator := l.mediator
	l.mediator = nil
	l.proxyURL = ""
	l.mu.Unlock()
	if mediator != nil {
		_ = mediator.Close()
	}
}

// Stop signals every process attributed to the run directly (TERM, grace,
// KILL), asks systemd to stop the unit as a courtesy, and verifies by
// inventory. Cleanup, mediator closure and serialization release are one
// serialized operation; concurrent callers cannot release a new generation.
func (l *LinuxManager) Stop(h Handle, reason string, grace time.Duration) (StopReceipt, error) {
	rc := StopReceipt{Run: h.Run, Reason: reason, StartedAt: time.Now()}
	finish := func(err error) (StopReceipt, error) {
		rc.FinishedAt = time.Now()
		return rc, err
	}
	l.cleanupMu.Lock()
	defer l.cleanupMu.Unlock()
	l.mu.Lock()
	if !l.authenticateHandleLocked(h) {
		l.mu.Unlock()
		return finish(errors.New("unknown or stale handle"))
	}
	launcher := l.launcher
	ef := l.mgrEnvFile
	l.mu.Unlock()
	l.closeMediator(h)
	if err := reapLinuxLauncher(launcher, l.reapTimeout()); err != nil {
		return finish(fmt.Errorf("stop %s: launcher reaping: %w", h.Run, err))
	}
	// A delayed launcher may not have read its environment yet. Keep the file
	// until reaping succeeds, even when a later cleanup step fails. Deletion is
	// part of verified cleanup: failure retains the handle, flock and run gate.
	if ef != "" {
		if err := l.removeEnvFile(ef); err != nil {
			return finish(fmt.Errorf("stop %s: worker environment cleanup: %w", h.Run, err))
		}
		l.clearEnvFile(ef)
	}
	if launcher == nil {
		// Only the prelaunch registration has no launcher. cmd.Start returned a
		// definite exec failure, so no unit or descendant needs inventory. The
		// environment file is gone; verified cleanup can reopen serialization.
		if h.PID != 0 {
			return finish(fmt.Errorf("stop %s: prelaunch registration has nonzero pid %d", h.Run, h.PID))
		}
		if err := l.releaseRunLocked(); err != nil {
			return finish(fmt.Errorf("stop %s: prelaunch cleanup unlock incomplete: %w", h.Run, err))
		}
		rc.Verified = true
		return finish(nil)
	}
	procs, err := l.Inventory(h)
	if err != nil {
		rc.Verified = false
		return finish(fmt.Errorf("stop %s: inventory(first): %w", h.Run, err))
	}
	if err := l.signalAll(procs, syscall.SIGTERM, &rc); err != nil {
		return finish(fmt.Errorf("stop %s: TERM: %w", h.Run, err))
	}
	_, _ = l.managerOutput("systemctl", "--user", "kill", "--signal=SIGTERM", h.Unit) // bounded courtesy; not trusted
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
		if err := l.signalAll(inv, syscall.SIGKILL, &rc); err != nil {
			return finish(fmt.Errorf("stop %s: KILL: %w", h.Run, err))
		}
		time.Sleep(150 * time.Millisecond)
	}
	if err := l.stopAndVerifyUnit(h); err != nil {
		return finish(fmt.Errorf("stop %s: late-unit cleanup: %w", h.Run, err))
	}
	rc.Survivors, err = l.Inventory(h)
	if err != nil {
		return finish(fmt.Errorf("stop %s: final inventory: %w", h.Run, err))
	}
	if len(rc.Survivors) != 0 {
		return finish(fmt.Errorf("stop %s: %d survivors", h.Run, len(rc.Survivors)))
	}
	if err := l.releaseRunLocked(); err != nil {
		rc.Verified = false
		return finish(fmt.Errorf("stop %s: cleanup unlock incomplete: %w", h.Run, err))
	}
	rc.Verified = true
	return finish(nil)
}

// mgrEscapeScript double-detaches a sleeper into its own session: the N6 shape.
const mgrEscapeScript = `python3 -c 'import os,time
os.setsid()
time.sleep(300)' </dev/null >/dev/null 2>&1 &
sleep 300`

// runControl runs one shell probe inside the profile and returns its output.
func (l *LinuxManager) runControl(g Grant, script string) (string, int, error) {
	outPath := filepath.Join(g.RunDir, fmt.Sprintf("control-%d.out", time.Now().UnixNano()))
	out, err := os.Create(outPath)
	if err != nil {
		return "", -1, err
	}
	h, err := l.Start(freshRun(g), []string{"sh", "-c", script}, nil, out, out)
	if err != nil {
		out.Close()
		return "", -1, err
	}
	code, _ := l.Wait(h)
	if _, stopErr := l.Stop(h, "negative-control cleanup", 2*time.Second); stopErr != nil {
		out.Close()
		return "", -1, stopErr
	}
	out.Close()
	b, _ := os.ReadFile(outPath)
	return strings.TrimSpace(string(b)), code, nil
}

// linuxEgressControlScript runs inside the worker. The provider argument is a
// numeric address resolved by the host, so the direct-connect test cannot be
// satisfied by a worker-side DNS failure. No provider credentials are used.
func linuxEgressControlScript(providerIP string) string {
	return fmt.Sprintf(`python3 -c '
import errno, os, socket, sys, urllib.parse
proxy_value = os.environ.get("HTTPS_PROXY", "")
proxy = urllib.parse.urlparse(proxy_value)
for name in ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"):
    if os.environ.get(name) != proxy_value:
        print("proxy_env=mismatch:" + name)
        sys.exit(0)
if os.environ.get("NO_PROXY") != "" or os.environ.get("no_proxy") != "":
    print("proxy_env=bypass-list")
    sys.exit(0)
if proxy.scheme != "http" or proxy.hostname != "127.0.0.1" or proxy.port is None:
    print("proxy_env=not-loopback")
    sys.exit(0)
print("proxy_env=exact")
print("proxy_url=" + proxy_value)
try:
    direct = socket.create_connection((sys.argv[1], 443), 2)
    print("direct=reachable")
    direct.close()
except OSError as e:
    print("direct=denied" if e.errno in (errno.EACCES, errno.EPERM) else "direct=error:" + str(e.errno))
def datagram(label, payload):
    try:
        client = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        client.settimeout(1)
        client.sendto(payload, ("127.0.0.1", 53))
        print(label + "=reachable")
        client.close()
    except OSError as e:
        print(label + ("=denied" if e.errno in (errno.EACCES, errno.EPERM) else "=error:" + str(e.errno)))
datagram("udp", b"worker-udp")
datagram("dns", b"\x12\x34\x01\x00\x00\x01\x00\x00\x00\x00\x00\x00\x00\x00\x01\x00\x01")
def connect(target):
    client = socket.create_connection(("127.0.0.1", proxy.port), 10)
    client.settimeout(10)
    request = "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n\r\n"
    client.sendall(request.encode("ascii"))
    status = client.makefile("rb").readline().decode("ascii", "replace").strip()
    client.close()
    return status
status = connect("api.anthropic.com:443")
print("proxy=allowed" if status.startswith("HTTP/1.1 200 ") else "proxy=denied:" + status)
for target in ("example.com:443", "127.0.0.1:" + str(proxy.port), "api.anthropic.com:443@example.com"):
    status = connect(target)
    print(("hostile_denied=" if status.startswith("HTTP/1.1 403 ") else "hostile_allowed=") + target)
' %q`, providerIP)
}

// NegativeControls proves (or disproves) the Linux boundaries on this host:
// process cleanup, filesystem and TCP restrictions, mediator-only egress,
// manager sibling prevention, and signal isolation. The signal control is
// expected to fail on a same-UID host without a dedicated worker account or
// kernel signal isolation. Every failure is reported.
func (l *LinuxManager) NegativeControls(scratch string) ([]ControlResult, error) {
	var out []ControlResult
	add := func(name string, ok bool, detail string) { out = append(out, ControlResult{name, ok, detail}) }
	g := mgrControlGrant(scratch)
	devnull, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	defer devnull.Close()

	// 1. control: a group signal does not reach a setsid grandchild.
	ctl := exec.Command("sh", "-c", mgrEscapeScript)
	ctl.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := ctl.Start(); err != nil {
		return nil, err
	}
	time.Sleep(400 * time.Millisecond)
	escapee := mgrFindChildSleeper(ctl.Process.Pid)
	_ = syscall.Kill(-ctl.Process.Pid, syscall.SIGKILL)
	_, _ = ctl.Process.Wait()
	time.Sleep(200 * time.Millisecond)
	survived := escapee > 0 && syscall.Kill(escapee, 0) == nil
	if survived {
		_ = syscall.Kill(escapee, syscall.SIGKILL) // ours: started by this control
	}
	add("group_signal_misses_setsid_grandchild", survived, fmt.Sprintf("escapee pid=%d survived=%v (expected true: a group is not containment)", escapee, survived))

	// 2. the same shape inside a unit is reaped by cgroup emptiness.
	h, err := l.Start(freshRun(g), []string{"sh", "-c", mgrEscapeScript}, nil, devnull, devnull)
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
	small := &LinuxManager{Self: l.Self, MemoryMax: "64M", ReadOnly: l.ReadOnly, LockPath: l.LockPath, EgressConfig: l.EgressConfig, User: l.User}
	h, err = small.Start(freshRun(g), []string{"python3", "-c", "x = bytearray(300 * 1024 * 1024); print(len(x))"}, nil, devnull, devnull)
	if err != nil {
		return out, err
	}
	code, _ := small.Wait(h)
	if _, stopErr := small.Stop(h, "memory-control cleanup", 2*time.Second); stopErr != nil {
		return out, stopErr
	}
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

	// The production egress boundary is mediator-only. These are real worker
	// connections, including one allowed provider tunnel and several hostile
	// CONNECT authorities. Missing proxy configuration is a failed control,
	// never a direct-egress fallback.
	if l.EgressConfig == nil {
		for _, name := range []string{"per_run_proxy_environment_exact", "direct_provider_egress_denied", "worker_udp_denied", "worker_local_dns_denied", "allowed_proxy_tunnel", "hostile_proxy_targets_denied", "per_run_proxy_cleanup"} {
			add(name, false, "per-run egress mediator is not configured")
		}
	} else {
		ips, lookupErr := net.LookupIP("api.anthropic.com")
		if lookupErr != nil {
			return out, fmt.Errorf("resolve direct-egress control target: %w", lookupErr)
		}
		providerIP := ""
		for _, ip := range ips {
			if ip.To4() != nil {
				providerIP = ip.String()
				break
			}
		}
		if providerIP == "" {
			return out, errors.New("direct-egress control target has no IPv4 address")
		}
		res, _, err = l.runControl(g, linuxEgressControlScript(providerIP))
		if err != nil {
			return out, err
		}
		add("per_run_proxy_environment_exact", strings.Contains(res, "proxy_env=exact"), res)
		add("direct_provider_egress_denied", strings.Contains(res, "direct=denied"), res)
		add("worker_udp_denied", strings.Contains(res, "udp=denied"), res)
		add("worker_local_dns_denied", strings.Contains(res, "dns=denied"), res)
		add("allowed_proxy_tunnel", strings.Contains(res, "proxy=allowed"), res)
		add("hostile_proxy_targets_denied", strings.Count(res, "hostile_denied=") == 3 && !strings.Contains(res, "hostile_allowed="), res)
		proxyURL := ""
		for _, line := range strings.Split(res, "\n") {
			if strings.HasPrefix(line, "proxy_url=http://127.0.0.1:") {
				proxyURL = strings.TrimPrefix(line, "proxy_url=")
				break
			}
		}
		cleanupErr := error(nil)
		if proxyURL == "" {
			cleanupErr = errors.New("worker did not report its proxy URL")
		} else {
			var conn net.Conn
			conn, cleanupErr = net.DialTimeout("tcp", strings.TrimPrefix(proxyURL, "http://"), 250*time.Millisecond)
			if cleanupErr == nil {
				_ = conn.Close()
			}
		}
		add("per_run_proxy_cleanup", proxyURL != "" && cleanupErr != nil, fmt.Sprintf("proxy=%s post-stop dial error=%v", proxyURL, cleanupErr))
	}

	// 8. Neither manager client nor a manually supplied bus address may create
	// a sibling outside this unit's cgroup, Landlock filesystem, and TCP rules.
	// Exercise names that old inventory policies exempted: fab-job, dbus and
	// session variants. Every attempt must fail; a missing unit alone is not
	// accepted as proof because the direct manager `show` must fail as well.
	workerUID, err := l.workerUID()
	if err != nil {
		return out, err
	}
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	siblings := []struct {
		name  string
		scope bool
	}{
		{"fab-job-" + suffix + ".service", false},
		{"dbus-" + suffix + ".service", false},
		{"session-" + suffix + ".scope", true},
	}
	var probe strings.Builder
	probe.WriteString(`try_manager() {
    label=$1
    shift
    if "$@" >/dev/null 2>&1; then
        printf 'sibling=created:%s\n' "$label"
    else
        printf 'sibling=denied:%s\n' "$label"
    fi
}
`)
	abstractTarget := "@fab-abstract-manager-" + suffix
	nonstandardTarget := filepath.Join(scratch, "nonstandard-manager-bus")
	fmt.Fprintf(&probe, `python3 -c '
import errno, socket, sys
for label, target in (("abstract", sys.argv[1]), ("nonstandard", sys.argv[2])):
    try:
        client = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        if target.startswith("@"):
            target = "\0" + target[1:]
        client.connect(target)
        print(label + "_unix=reachable")
        client.close()
    except OSError as e:
        print(label + "_unix=" + ("denied" if e.errno in (errno.EACCES, errno.EPERM) else "error:" + str(e.errno)))
' %q %q
`, abstractTarget, nonstandardTarget)
	attempts := 0
	for _, sibling := range siblings {
		if sibling.scope {
			fmt.Fprintf(&probe, "try_manager %q systemd-run --user --scope --slice=session.slice --collect --quiet --unit %q -- /bin/true\n", "run:"+sibling.name, sibling.name)
		} else {
			fmt.Fprintf(&probe, "try_manager %q systemd-run --user --wait --collect --quiet --unit %q -- /bin/true\n", "run:"+sibling.name, sibling.name)
		}
		attempts++
		fmt.Fprintf(&probe, "try_manager %q systemctl --user start %q\n", "ctl:"+sibling.name, sibling.name)
		attempts++
	}
	manualUnit := "fab-manual-bus-" + suffix + ".service"
	runtimeDir := fmt.Sprintf("/run/user/%d", workerUID)
	busAddress := "unix:path=" + runtimeDir + "/bus"
	fmt.Fprintf(&probe, "try_manager %q env XDG_RUNTIME_DIR=%q DBUS_SESSION_BUS_ADDRESS=%q systemd-run --user --wait --collect --quiet --unit %q -- /bin/true\n", "manual:"+manualUnit, runtimeDir, busAddress, manualUnit)
	attempts++
	fmt.Fprintf(&probe, "try_manager %q env XDG_RUNTIME_DIR=%q DBUS_SESSION_BUS_ADDRESS=%q systemctl --user start %q\n", "manual-ctl:"+manualUnit, runtimeDir, busAddress, manualUnit)
	attempts++
	fmt.Fprintf(&probe, "try_manager %q env XDG_RUNTIME_DIR=%q DBUS_SESSION_BUS_ADDRESS=%q systemctl --user show\n", "manager-show", runtimeDir, busAddress)
	attempts++
	busCases := []struct{ label, address, unit string }{
		{"abstract-bus", "unix:abstract=fab-abstract-manager-" + suffix, "fab-abstract-bus-" + suffix + ".service"},
		{"nonstandard-bus", "unix:path=" + nonstandardTarget, "fab-nonstandard-bus-" + suffix + ".service"},
	}
	for _, bus := range busCases {
		fmt.Fprintf(&probe, "try_manager %q env DBUS_SESSION_BUS_ADDRESS=%q systemd-run --user --wait --collect --quiet --unit %q -- /bin/true\n", bus.label, bus.address, bus.unit)
		attempts++
	}
	res, _, err = l.runControl(g, probe.String())
	for _, sibling := range siblings {
		_ = l.runAsWorker("systemctl", "--user", "stop", sibling.name)
	}
	_ = l.runAsWorker("systemctl", "--user", "stop", manualUnit)
	for _, bus := range busCases {
		_ = l.runAsWorker("systemctl", "--user", "stop", bus.unit)
	}
	if err != nil {
		return out, err
	}
	denied := strings.Count(res, "sibling=denied:")
	add("controlling_user_manager_unreachable", denied == attempts && !strings.Contains(res, "sibling=created:"),
		fmt.Sprintf("%d/%d manager attempts denied; %s", denied, attempts, res))
	add("abstract_and_nonstandard_unix_sockets_denied", strings.Contains(res, "abstract_unix=denied") && strings.Contains(res, "nonstandard_unix=denied"), res)

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

// mgrFindChildSleeper returns the pid of the python sleeper descended from
// parent (it re-parents after its parent sh returns, so walk /proc).
func mgrFindChildSleeper(parent int) int {
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		p, ok, err := mgrProcIdentity(pid)
		if err != nil || !ok || !strings.HasPrefix(p.Command, "python3 -c ") || !strings.Contains(p.Command, "os.setsid()") {
			continue
		}
		if p.PGID == pid && p.PGID != parent {
			return pid
		}
	}
	return 0
}

// mgrControlGrant is controlGrant plus the run/jobs directory the manager
// uses for its readiness marker and environment file (sched creates it for
// real runs). The legacy controlGrant is unchanged (goodqa#15235 S5b).
func mgrControlGrant(scratch string) Grant {
	g := controlGrant(scratch)
	jobs := filepath.Join(g.RunDir, "jobs")
	os.MkdirAll(jobs, 0o775)                //nolint:errcheck
	os.Chmod(jobs, 0o775|os.ModeSetgid)     //nolint:errcheck
	os.Chmod(g.RunDir, 0o750|os.ModeSetgid) //nolint:errcheck
	return g
}

// ErrLinuxManagerUnsupported means this host cannot enforce the worker
// manager's procfs isolation. Start refuses; nothing falls back to a weaker
// boundary or to the legacy profile (goodqa#15235 S5b).
var ErrLinuxManagerUnsupported = errors.New("linux manager unsupported")

// minManagerSystemd is the first systemd with PrivatePIDs (v257).
const minManagerSystemd = 257

// managerLogf records every unsupported-host refusal; a variable for tests.
var managerLogf = log.Printf

func refuseUnsupported(reason string) error {
	err := fmt.Errorf("%w: %s", ErrLinuxManagerUnsupported, reason)
	managerLogf("fab: %v", err)
	return err
}

// parseSystemdVersion reads the major version from `systemctl --version`.
func parseSystemdVersion(out string) (int, bool) {
	line, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	fields := strings.Fields(line)
	if len(fields) < 2 || fields[0] != "systemd" {
		return 0, false
	}
	v, err := strconv.Atoi(fields[1])
	if err != nil || v <= 0 {
		return 0, false
	}
	return v, true
}

// checkManagerSupport refuses before any unit starts when systemd is too old
// for PrivatePIDs, or when its version cannot be read (fail closed).
func (l *LinuxManager) checkManagerSupport() error {
	ctx, cancel := context.WithTimeout(context.Background(), l.managerTimeout())
	defer cancel()
	out, err := l.command(ctx, "systemctl", "--version").Output()
	if err != nil {
		return refuseUnsupported(fmt.Sprintf("cannot read the systemd version: %v", err))
	}
	v, ok := parseSystemdVersion(string(out))
	if !ok {
		return refuseUnsupported("cannot parse the systemd version")
	}
	if v < minManagerSystemd {
		return refuseUnsupported(fmt.Sprintf("PrivatePIDs needs systemd>=%d (found %d); procfs isolation not enforced in user units", minManagerSystemd, v))
	}
	return nil
}

// unsupportedPreflightReason maps preflight output that proves the host
// cannot enforce procfs isolation to an explicit reason; "" otherwise.
func unsupportedPreflightReason(out string) string {
	switch {
	case strings.Contains(out, "Unknown assignment: PrivatePIDs"):
		return "PrivatePIDs is not supported by this systemd; procfs isolation not enforced in user units"
	case strings.Contains(out, "PrivatePIDs is ineffective"):
		return "PrivatePIDs was accepted but not enforced; procfs isolation not enforced in user units"
	case strings.Contains(out, "ProcSubset=pid is ineffective"):
		return "ProtectProc/ProcSubset are not enforced in user units"
	}
	return ""
}
