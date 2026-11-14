package host

import (
	"os"
	"strings"
	"testing"
)

// This guard runs on every development host, including non-Linux hosts where
// the Linux unit tests cannot execute. Keep the containment operations visible
// even when launch, readiness and proc parsing are split across Linux files.
func TestLinuxSourceHasNoFabJobPrefixInventoryExemption(t *testing.T) {
	var source string
	for _, name := range []string{"linux_manager.go", "linux_manager_proc_linux.go", "linux_manager_readiness_linux.go", "linux_manager_launcher_linux.go"} {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		source += string(b)
	}
	forbidden := `strings.HasPrefix(rel, "app.slice/fab-job-")`
	if strings.Contains(source, forbidden) {
		t.Fatal("Linux inventory directly exempts fab-job-* siblings")
	}
	for _, required := range []string{
		"workerManagerEscape(rel, unitRel)",
		"mgrUidProcesses(uid, inside, h.CGroup)",
		"l.runGate.tryLock()",
		"l.lockWorkerManager(uid)",
		"assertNoAbandonedRuns(uid)",
		"l.preflightUnit(uid, policy)",
		"egress.Start(context.Background(), *l.EgressConfig)",
		"l.waitUnitReady(h, readyFile, uid)",
		"readLinuxReadyMarker(readyFile, workerUID)",
		"workerPrivatePIDsProperty",
		"workerProtectProcProperty",
		"workerProcSubsetProperty",
		"reapLinuxLauncher(launcher, l.reapTimeout())",
		"l.stopAndVerifyUnit(h)",
		"rc.Survivors, err = l.verifyStopped(h)",
		"baseline, err := l.workerUnits()",
		"escapedWorkerUnits(baseline, now, h.Unit)",
		"l.managerOutput(",
		"return h, fmt.Errorf(",
		"cgroupHasLiveProcess(h.CGroup)",
		"l.closeMediator(h)",
		"l.authenticateHandleLocked(h)",
		"managerInaccessiblePathsProperty(uid)",
		"managerSocketFamilyProperty",
		"fab-job-",
		"dbus-",
		"abstract_unix=denied",
		"nonstandard_unix=denied",
		"udp=denied",
		"dns=denied",
		"DBUS_SESSION_BUS_ADDRESS",
		"l.releaseRunLocked()",
		"UnitSerial: serial",
		"mgrProcIdentity(pid)",
		"procUIDAt(root, pid)",
	} {
		if !strings.Contains(source, required) {
			t.Fatalf("Linux source lacks containment operation %q", required)
		}
	}
	lockSource, err := os.ReadFile("linux_manager_lock_linux.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"l.activeSerial = 0", "l.active = Handle{}", "lockInodeMatches", "Mode().Perm() != 0o700", "owner.Nlink != 1"} {
		if !strings.Contains(string(lockSource), required) {
			t.Fatalf("Linux lock release lacks %q", required)
		}
	}
	if strings.Contains(source, "l.runs.") {
		t.Fatal("linux.go reintroduced a unit-name registry or snapshot")
	}
	landlockSource, err := os.ReadFile("landlock_linux.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(landlockSource), "return 4") || !strings.Contains(string(landlockSource), "if managerContainment(sb) {\n\t\tif err := ApplyWorkerSocketPolicy()") || !strings.Contains(string(landlockSource), "func CompleteSandboxReady(sb Sandbox)") {
		t.Fatal("Linux launch no longer enforces protocol containment and durable readiness")
	}
	seccompSource, err := os.ReadFile("linux_manager_seccomp_linux.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, syscallName := range []string{"SYS_PTRACE", "SYS_PROCESS_VM_WRITEV", "SYS_PIDFD_GETFD", "SYS_PIDFD_SEND_SIGNAL", "SYS_KILL"} {
		if !strings.Contains(string(seccompSource), syscallName) {
			t.Fatalf("worker seccomp lacks cross-process denial %s", syscallName)
		}
	}
}
