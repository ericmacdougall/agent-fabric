//go:build linux

package main

import (
	"os"
	"strings"
	"testing"
)

// goodqa#15235 S5b delta red-team D1: sandboxMain pins its OS thread before
// any per-thread restriction is applied.
func TestSandboxMainLocksOSThreadBeforeLandlock(t *testing.T) {
	src, err := os.ReadFile("os_linux.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	start := strings.Index(s, "func sandboxMain(args []string) error {")
	if start < 0 {
		t.Fatal("sandboxMain not found")
	}
	body := s[start:]
	lock := strings.Index(body, "runtime.LockOSThread()")
	for _, applied := range []string{"host.ApplyWorkerSocketPolicy()", "host.ApplyLandlock(sb)", "syscall.Exec("} {
		at := strings.Index(body, applied)
		if at < 0 {
			t.Fatalf("sandboxMain lacks %s", applied)
		}
		if lock < 0 || lock > at {
			t.Fatalf("runtime.LockOSThread() must precede %s in sandboxMain", applied)
		}
	}
	if strings.Contains(body[:strings.Index(body, "\n}\n")], "UnlockOSThread") {
		t.Fatal("sandboxMain must never unlock its OS thread")
	}
}
