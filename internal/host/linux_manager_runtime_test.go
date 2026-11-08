//go:build linux

package host

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func runtimeLinuxProfile(base *LinuxManager) *LinuxManager {
	return &LinuxManager{
		Self: base.Self, MemoryMax: base.MemoryMax, CPUQuota: base.CPUQuota,
		TasksMax: base.TasksMax, ReadOnly: base.ReadOnly, ExtraRW: base.ExtraRW,
		TCPPorts: base.TCPPorts, LockPath: base.LockPath, EgressConfig: base.EgressConfig,
		User: base.User, commandFn: base.commandFn, managerCallTimeout: base.managerCallTimeout,
		unitReadinessTimeout: base.unitReadinessTimeout, launcherReapTimeout: base.launcherReapTimeout,
	}
}

func runtimeStart(t *testing.T, l *LinuxManager, argv ...string) (Handle, error) {
	t.Helper()
	out, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	return l.Start(controlGrant(t.TempDir()), argv, nil, out, out)
}

func requireRuntimeProxyClosed(t *testing.T, proxyURL string) {
	t.Helper()
	if !strings.HasPrefix(proxyURL, "http://127.0.0.1:") {
		t.Fatalf("active run has no numeric loopback proxy: %q", proxyURL)
	}
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(proxyURL, "http://"), 250*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		t.Fatalf("per-run proxy %s remained reachable after Stop", proxyURL)
	}
}

// TestLinuxRuntimeSerializationCleanupFailover is a real systemd/cgroup test,
// not a compile-time control. Stale serials and forged same-serial identities
// must not open serialization or close the active mediator. Concurrent Stop
// calls must produce exactly one verified cleanup.
func TestLinuxRuntimeSerializationCleanupFailover(t *testing.T) {
	if os.Getenv("FABRIC_HOST_CONTROLS") == "" {
		t.Skip("set FABRIC_HOST_CONTROLS=1 to run Linux runtime serialization controls")
	}
	first := testProfile(t).(*LinuxManager)
	second := runtimeLinuxProfile(first)
	third := runtimeLinuxProfile(first)

	h1, err := runtimeStart(t, first, "sleep", "30")
	if err != nil {
		t.Fatal(err)
	}
	proxy1 := first.currentProxyURL()
	defer first.Stop(h1, "runtime test final cleanup", time.Second) //nolint:errcheck
	if _, err := runtimeStart(t, second, "sleep", "1"); !errors.Is(err, ErrWorkerManagerBusy) {
		t.Fatalf("concurrent profile start: got %v, want ErrWorkerManagerBusy", err)
	}
	stale := h1
	stale.UnitSerial--
	rc, err := first.Stop(stale, "stale handle must not clean up current run", 0)
	if err == nil || rc.Verified {
		t.Fatalf("stale Stop returned err=%v verified=%v; current generation must remain locked", err, rc.Verified)
	}
	forged := h1
	forged.Unit = "fab-forged-same-serial.service"
	rc, err = first.Stop(forged, "forged same-serial handle", 0)
	if err == nil || rc.Verified {
		t.Fatalf("same-serial forged Stop returned err=%v verified=%v", err, rc.Verified)
	}
	if got := first.currentProxyURL(); got != proxy1 || got == "" {
		t.Fatalf("stale Stop changed the active proxy: before=%q after=%q", proxy1, got)
	}
	if _, err := runtimeStart(t, second, "sleep", "1"); !errors.Is(err, ErrWorkerManagerBusy) {
		t.Fatalf("stale Stop released serialization: got %v, want ErrWorkerManagerBusy", err)
	}
	rc, err = first.Stop(h1, "verified first cleanup", 2*time.Second)
	if err != nil || !rc.Verified {
		t.Fatalf("first verified cleanup: err=%v receipt=%+v", err, rc)
	}
	requireRuntimeProxyClosed(t, proxy1)

	h2, err := runtimeStart(t, second, "sleep", "1")
	if err != nil {
		t.Fatalf("start after verified cleanup failed: %v", err)
	}
	proxy2 := second.currentProxyURL()
	defer second.Stop(h2, "runtime test final cleanup", time.Second) //nolint:errcheck
	if _, err := second.Wait(h2); err != nil {
		t.Fatalf("second Wait: %v", err)
	}
	if _, err := runtimeStart(t, third, "sleep", "1"); !errors.Is(err, ErrWorkerManagerBusy) {
		t.Fatalf("leader exit released cleanup gate: got %v, want ErrWorkerManagerBusy", err)
	}
	rc, err = second.Stop(h2, "verified post-exit cleanup", 2*time.Second)
	if err != nil || !rc.Verified {
		t.Fatalf("post-exit verified cleanup: err=%v receipt=%+v", err, rc)
	}
	requireRuntimeProxyClosed(t, proxy2)

	h3, err := runtimeStart(t, third, "sleep", "30")
	if err != nil {
		t.Fatalf("failover after post-exit cleanup failed: %v", err)
	}
	proxy3 := third.currentProxyURL()
	if proxy3 == "" {
		t.Fatal("failover run did not create a fresh mediator")
	}
	type stopResult struct {
		rc  StopReceipt
		err error
	}
	results := make(chan stopResult, 2)
	for i := 0; i < 2; i++ {
		go func() {
			rc, err := third.Stop(h3, "concurrent verified failover cleanup", 2*time.Second)
			results <- stopResult{rc: rc, err: err}
		}()
	}
	verified, rejected := 0, 0
	for i := 0; i < 2; i++ {
		result := <-results
		if result.err == nil && result.rc.Verified && len(result.rc.Survivors) == 0 {
			verified++
		} else if result.err != nil && !result.rc.Verified {
			rejected++
		} else {
			t.Fatalf("unexpected concurrent Stop result: %+v", result)
		}
	}
	if verified != 1 || rejected != 1 {
		t.Fatalf("concurrent Stop verified=%d rejected=%d, want exactly one of each", verified, rejected)
	}
	requireRuntimeProxyClosed(t, proxy3)
}

// TestLinuxRuntimeDelayedManagerReadiness delays the real systemd-run command.
// Start must not return a handle until the exact unit is active and populated;
// an immediate Stop then has real processes to observe and reap.
func TestLinuxRuntimeDelayedManagerReadiness(t *testing.T) {
	if os.Getenv("FABRIC_HOST_CONTROLS") == "" {
		t.Skip("set FABRIC_HOST_CONTROLS=1 to run delayed-manager readiness control")
	}
	profile := testProfile(t).(*LinuxManager)
	profile.commandFn = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		delay := name == "systemd-run"
		for _, arg := range args {
			delay = delay || arg == "systemd-run"
		}
		if delay {
			command := append([]string{"-c", `sleep 0.25; exec "$@"`, "sh", name}, args...)
			return exec.CommandContext(ctx, "sh", command...)
		}
		return exec.CommandContext(ctx, name, args...)
	}
	started := time.Now()
	h, err := runtimeStart(t, profile, "sleep", "30")
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < 250*time.Millisecond {
		t.Fatalf("Start returned before the delayed manager could realize the unit: %v", elapsed)
	}
	inventory, err := profile.Inventory(h)
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory) == 0 {
		t.Fatal("Start returned with an empty unit cgroup after the readiness barrier")
	}
	rc, err := profile.Stop(h, "immediate Stop after delayed-manager readiness", 2*time.Second)
	if err != nil || !rc.Verified || len(rc.Survivors) != 0 {
		t.Fatalf("immediate Stop after readiness: receipt=%+v err=%v", rc, err)
	}
}

// TestLinuxRuntimeWorkerCannotReplaceManagerLock gives the worker the exact
// lock pathname. The controller-owned 0700 directory must make replacement
// fail with EACCES/EPERM, not merely detect it after a successful attack.
func TestLinuxRuntimeWorkerCannotReplaceManagerLock(t *testing.T) {
	if os.Getenv("FABRIC_HOST_CONTROLS") == "" {
		t.Skip("set FABRIC_HOST_CONTROLS=1 to run lock replacement control")
	}
	profile := testProfile(t).(*LinuxManager)
	profile.LockPath = filepath.Join(t.TempDir(), "worker-manager.lock")
	outputPath := filepath.Join(t.TempDir(), "lock-control.out")
	output, err := os.Create(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf(`python3 -c 'import errno, os, sys
try:
    os.rename(sys.argv[1], sys.argv[1] + ".worker-replaced")
    print("lock=replaced")
except OSError as e:
    print("lock=" + ("denied" if e.errno in (errno.EACCES, errno.EPERM) else "error:" + str(e.errno)))
' %q; sleep 1`, profile.LockPath)
	h, err := profile.Start(controlGrant(t.TempDir()), []string{"sh", "-c", script}, nil, output, output)
	_ = output.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer profile.Stop(h, "lock replacement final cleanup", time.Second) //nolint:errcheck
	if _, err := profile.Wait(h); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "lock=denied") {
		t.Fatalf("worker lock replacement was not denied by policy: %s", out)
	}
	rc, err := profile.Stop(h, "verified lock replacement cleanup", 2*time.Second)
	if err != nil || !rc.Verified {
		t.Fatalf("lock replacement cleanup: receipt=%+v err=%v", rc, err)
	}
}

func runtimeWorkerSystemdRun(name string, args []string) bool {
	found := name == "systemd-run"
	unit := ""
	for i, arg := range args {
		if arg == "systemd-run" {
			found = true
		}
		if arg == "--unit" && i+1 < len(args) {
			unit = args[i+1]
		}
	}
	return found && unit != "" && !strings.HasPrefix(unit, "fab-manager-preflight-")
}

func TestLinuxRuntimeDelayedSubmissionCannotEscapeCleanup(t *testing.T) {
	if os.Getenv("FABRIC_HOST_CONTROLS") == "" {
		t.Skip("set FABRIC_HOST_CONTROLS=1 to run delayed submission control")
	}
	profile := testProfile(t).(*LinuxManager)
	profile.unitReadinessTimeout = 250 * time.Millisecond
	profile.managerCallTimeout = 500 * time.Millisecond
	profile.launcherReapTimeout = 500 * time.Millisecond
	profile.commandFn = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if runtimeWorkerSystemdRun(name, args) {
			command := append([]string{"-c", `sleep 2; exec "$@"`, "sh", name}, args...)
			return exec.CommandContext(ctx, "sh", command...)
		}
		return exec.CommandContext(ctx, name, args...)
	}
	started := time.Now()
	h, err := runtimeStart(t, profile, "sleep", "30")
	if err == nil {
		if h.UnitSerial != 0 {
			_, _ = profile.Stop(h, "unexpected delayed submission success", time.Second)
		}
		t.Fatal("delayed submission unexpectedly became ready")
	}
	if h.UnitSerial != 0 {
		t.Fatalf("successful delayed-unit cleanup lost its completion state: handle=%+v err=%v", h, err)
	}
	if elapsed := time.Since(started); elapsed > 1800*time.Millisecond {
		t.Fatalf("delayed submission cleanup exceeded its bounded lifetime: %v", elapsed)
	}
	time.Sleep(1200 * time.Millisecond) // beyond the uncancelled submission delay
	uid, err := profile.workerUID()
	if err != nil {
		t.Fatal(err)
	}
	if err := assertNoAbandonedRuns(uid); err != nil {
		t.Fatalf("cancelled submission created a late unit: %v", err)
	}
	second := runtimeLinuxProfile(profile)
	second.commandFn = nil
	h2, err := runtimeStart(t, second, "sleep", "1")
	if err != nil {
		t.Fatalf("delayed cleanup did not reopen serialization: %v", err)
	}
	if rc, err := second.Stop(h2, "post-delay failover", 2*time.Second); err != nil || !rc.Verified {
		t.Fatalf("post-delay failover cleanup: receipt=%+v err=%v", rc, err)
	}
}

func TestLinuxRuntimeBlockedManagerReturnsRetryableHandle(t *testing.T) {
	if os.Getenv("FABRIC_HOST_CONTROLS") == "" {
		t.Skip("set FABRIC_HOST_CONTROLS=1 to run blocked-manager timeout control")
	}
	profile := testProfile(t).(*LinuxManager)
	profile.managerCallTimeout = 150 * time.Millisecond
	profile.unitReadinessTimeout = 300 * time.Millisecond
	profile.launcherReapTimeout = 500 * time.Millisecond
	var blocked atomic.Bool
	blocked.Store(true)
	profile.commandFn = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		isSystemctl := name == "systemctl"
		for _, arg := range args {
			isSystemctl = isSystemctl || arg == "systemctl"
		}
		if blocked.Load() && isSystemctl {
			return exec.CommandContext(ctx, "sh", "-c", "sleep 30")
		}
		return exec.CommandContext(ctx, name, args...)
	}
	started := time.Now()
	h, err := runtimeStart(t, profile, "sleep", "30")
	if err == nil {
		if h.UnitSerial != 0 {
			_, _ = profile.Stop(h, "unexpected blocked-manager success", time.Second)
		}
		t.Fatal("blocked manager unexpectedly certified readiness")
	}
	if h.UnitSerial == 0 || h.Unit == "" || h.CGroup == "" {
		t.Fatalf("failed late-unit cleanup did not return an authenticated retry handle: %+v", h)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("manager calls did not honor their deadlines: %v", elapsed)
	}
	blocked.Store(false)
	rc, err := profile.Stop(h, "retry after manager recovery", 2*time.Second)
	if err != nil || !rc.Verified {
		t.Fatalf("authenticated cleanup retry failed: receipt=%+v err=%v", rc, err)
	}
}

func TestLinuxRuntimeFastSuccessfulCommand(t *testing.T) {
	if os.Getenv("FABRIC_HOST_CONTROLS") == "" {
		t.Skip("set FABRIC_HOST_CONTROLS=1 to run fast-command control")
	}
	profile := testProfile(t).(*LinuxManager)
	h, err := runtimeStart(t, profile, "/bin/true")
	if err != nil {
		t.Fatalf("fast successful command was misclassified: %v", err)
	}
	code, err := profile.Wait(h)
	if err != nil || code != 0 {
		t.Fatalf("preserved fast-command wait status: code=%d err=%v", code, err)
	}
	rc, err := profile.Stop(h, "fast successful command cleanup", 2*time.Second)
	if err != nil || !rc.Verified {
		t.Fatalf("fast successful command cleanup: receipt=%+v err=%v", rc, err)
	}
}

func TestLinuxRuntimeSameUIDProcessAttacksDenied(t *testing.T) {
	if os.Getenv("FABRIC_HOST_CONTROLS") == "" {
		t.Skip("set FABRIC_HOST_CONTROLS=1 to run same-UID process attack controls")
	}
	profile := testProfile(t).(*LinuxManager)
	targetContext, cancelTarget := context.WithCancel(context.Background())
	target := profile.asWorkerContext(targetContext, "sleep", "30")
	if err := target.Start(); err != nil {
		cancelTarget()
		t.Fatal(err)
	}
	defer func() {
		cancelTarget()
		_ = syscall.Kill(-target.Process.Pid, syscall.SIGKILL)
		_, _ = target.Process.Wait()
	}()
	g := controlGrant(t.TempDir())
	outputPath := filepath.Join(g.RunDir, "process-attacks.out")
	output, err := os.Create(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf(`python3 -c 'import ctypes, errno, os, sys
pid = int(sys.argv[1])
libc = ctypes.CDLL(None, use_errno=True)
libc.syscall.restype = ctypes.c_long
machine = os.uname().machine
numbers = {"x86_64": (101, 311, 434, 438), "aarch64": (117, 271, 434, 438)}
if machine not in numbers:
    print("syscall=unsupported")
    sys.exit(0)
ptrace_nr, process_vm_nr, pidfd_open_nr, pidfd_getfd_nr = numbers[machine]
try:
    os.kill(pid, 0)
    print("signal=allowed")
except OSError as e:
    print("signal=" + ("denied" if e.errno == errno.EPERM else "error:" + str(e.errno)))
for name in ("mem", "environ", "maps"):
    try:
        file = open("/proc/%%d/%%s" %% (pid, name), "rb", buffering=0)
        file.close()
        print(name + "=allowed")
    except OSError as e:
        print(name + "=" + ("denied" if e.errno in (errno.ENOENT, errno.EACCES, errno.EPERM) else "error:" + str(e.errno)))
def check(label, result):
    error = ctypes.get_errno()
    print(label + "=" + ("denied" if result < 0 and error == errno.EPERM else "allowed:" + str(result) + ":errno" + str(error)))
ctypes.set_errno(0)
check("ptrace", libc.syscall(ptrace_nr, 16, pid, 0, 0))
ctypes.set_errno(0)
check("pidfd_open", libc.syscall(pidfd_open_nr, pid, 0))
ctypes.set_errno(0)
check("pidfd_getfd", libc.syscall(pidfd_getfd_nr, -1, 0, 0))
class IOVec(ctypes.Structure):
    _fields_ = [("base", ctypes.c_void_p), ("length", ctypes.c_size_t)]
local_buffer = ctypes.create_string_buffer(b"x")
local = IOVec(ctypes.cast(local_buffer, ctypes.c_void_p), 1)
remote = IOVec(ctypes.c_void_p(1), 1)
ctypes.set_errno(0)
check("process_vm_writev", libc.syscall(process_vm_nr, pid, ctypes.byref(local), 1, ctypes.byref(remote), 1, 0))
' %q`, target.Process.Pid)
	h, err := profile.Start(g, []string{"sh", "-c", script}, nil, output, output)
	_ = output.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := profile.Wait(h); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"signal", "mem", "environ", "maps", "ptrace", "pidfd_open", "pidfd_getfd", "process_vm_writev"} {
		if !strings.Contains(string(out), label+"=denied") {
			t.Fatalf("same-UID %s attack was not denied by procfs/seccomp isolation: %s", label, out)
		}
	}
	if err := syscall.Kill(target.Process.Pid, 0); err != nil {
		t.Fatalf("sacrificial same-UID waiter was attacked: %v", err)
	}
	if rc, err := profile.Stop(h, "same-UID attack control cleanup", 2*time.Second); err != nil || !rc.Verified {
		t.Fatalf("same-UID attack cleanup: receipt=%+v err=%v", rc, err)
	}
}

func TestLinuxRuntimeDedicatedWorkerTraversal(t *testing.T) {
	if os.Getenv("FABRIC_HOST_CONTROLS") == "" {
		t.Skip("set FABRIC_HOST_CONTROLS=1 to run dedicated-worker traversal control")
	}
	profile := testProfile(t).(*LinuxManager)
	root := os.Getenv("FABRIC_WORKER_ROOT")
	if profile.User == "" || root == "" {
		t.Fatal("dedicated traversal requires FABRIC_WORKER_USER and FABRIC_WORKER_ROOT")
	}
	g := controlGrant(t.TempDir())
	outputPath := filepath.Join(g.RunDir, "traversal.out")
	output, err := os.Create(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	configDir := filepath.Join(g.RunDir, "claude-config")
	storageDir := filepath.Join(g.RunDir, "claude-login")
	for _, dir := range []string{configDir, storageDir} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(configDir, 0o775|os.ModeSetgid); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(storageDir, 0o750|os.ModeSetgid); err != nil {
		t.Fatal(err)
	}
	credential := filepath.Join(storageDir, ".credentials.json")
	if err := os.WriteFile(credential, []byte("fixture-only-never-a-real-token"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(credential, 0o640); err != nil {
		t.Fatal(err)
	}
	published := filepath.Join(configDir, ".credentials.json")
	if err := os.Symlink(filepath.Join("..", "claude-login", ".credentials.json"), published); err != nil {
		t.Fatal(err)
	}
	renamedPublished := filepath.Join(configDir, "worker-renamed-credentials")
	renamedCredential := filepath.Join(storageDir, "worker-renamed-credentials")
	renamedStorage := filepath.Join(g.RunDir, "worker-renamed-login")
	renamedConfig := filepath.Join(g.RunDir, "worker-renamed-config")
	stateFile := filepath.Join(configDir, "session.json")
	script := fmt.Sprintf(`set -eu
test -x %q
test "$(pwd)" = %q
printf worktree > %q
printf jobs > %q
printf state > %q
if cat %q >/dev/null; then echo credential=readable; else echo credential=unreadable; fi
if printf run > %q 2>/dev/null; then echo run_root=writable; else echo run_root=denied; fi
if printf root > %q 2>/dev/null; then echo root=writable; else echo root=denied; fi
if mv %q %q 2>/dev/null; then echo published_rename=allowed; else echo published_rename=denied; fi
if mv %q %q 2>/dev/null; then echo credential_rename=allowed; else echo credential_rename=denied; fi
if mv %q %q 2>/dev/null; then echo storage_rename=allowed; else echo storage_rename=denied; fi
if mv %q %q 2>/dev/null; then echo config_rename=allowed; else echo config_rename=denied; fi
echo traversal=ok`, profile.Self, g.Worktree, filepath.Join(g.Worktree, "worker-file"), filepath.Join(g.RunDir, "jobs", "worker-file"), stateFile, published,
		filepath.Join(g.RunDir, "worker-file"), filepath.Join(root, "worker-must-not-write"), published, renamedPublished, credential, renamedCredential, storageDir, renamedStorage, configDir, renamedConfig)
	h, err := profile.Start(g, []string{"sh", "-c", script}, nil, output, output)
	_ = output.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer profile.Stop(h, "dedicated traversal final cleanup", time.Second) //nolint:errcheck
	if _, err := profile.Wait(h); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"traversal=ok", "credential=readable", "run_root=denied", "root=denied", "published_rename=allowed", "credential_rename=denied", "storage_rename=denied", "config_rename=denied"} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("dedicated worker control %q failed: %s", want, out)
		}
	}
	for _, path := range []string{filepath.Join(g.Worktree, "worker-file"), filepath.Join(g.RunDir, "jobs", "worker-file"), stateFile, credential, storageDir, configDir, renamedPublished} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("required path %s is missing after worker control: %v", path, err)
		}
	}
	for _, path := range []string{renamedCredential, renamedStorage, renamedConfig} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("worker renamed a protected credential path %s: %v", path, err)
		}
	}
	// Controller cleanup of the one real credential invalidates the renamed view.
	if err := os.Remove(credential); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(storageDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(renamedPublished); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("renamed public view survived protected credential cleanup: %v", err)
	}
	if rc, err := profile.Stop(h, "dedicated traversal cleanup", 2*time.Second); err != nil || !rc.Verified {
		t.Fatalf("dedicated traversal cleanup: receipt=%+v err=%v", rc, err)
	}
}
