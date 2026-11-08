//go:build linux

package host

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

const seccompChildEnv = "FABRIC_TEST_SECCOMP_TSYNC_CHILD"

// goodqa#15235 S5b delta red-team D2: after ApplyWorkerSocketPolicy every
// thread of the process is filtered, not only the caller. Runs in a child
// process because the filter cannot be removed.
func TestWorkerSocketPolicyCoversEveryThread(t *testing.T) {
	if os.Getenv(seccompChildEnv) == "1" {
		seccompTSyncChild(t)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestWorkerSocketPolicyCoversEveryThread$", "-test.count=1")
	cmd.Env = append(os.Environ(), seccompChildEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "TSYNC-OK") {
		t.Fatalf("child: %v\n%s", err, out)
	}
}

func seccompTSyncChild(t *testing.T) {
	// Park several goroutines on their own OS threads before the filter.
	var ready, release sync.WaitGroup
	release.Add(1)
	for i := 0; i < 4; i++ {
		ready.Add(1)
		go func() {
			runtime.LockOSThread()
			ready.Done()
			release.Wait()
		}()
	}
	ready.Wait()
	runtime.LockOSThread()
	if err := ApplyWorkerSocketPolicy(); err != nil {
		t.Fatal(err)
	}
	tasks, err := filepath.Glob("/proc/self/task/*/status")
	if err != nil || len(tasks) < 5 {
		t.Fatalf("want at least 5 threads, got %d (%v)", len(tasks), err)
	}
	for _, status := range tasks {
		b, err := os.ReadFile(status)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "\nSeccomp:\t2\n") {
			t.Fatalf("%s is not in seccomp filter mode", status)
		}
	}
	release.Done()
	t.Log("TSYNC-OK")
	os.Stdout.WriteString("TSYNC-OK\n")
}
