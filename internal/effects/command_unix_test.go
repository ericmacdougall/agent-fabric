//go:build unix

package effects

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Review W2: a process-based executor (AC-12's git push) runs through
// RunCommand, which kills the whole process group at the deadline, so a
// grandchild cannot keep writing after the broker gave up.
func TestRunCommandKillsProcessGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := RunCommand(ctx, "", "sh", "-c", `sleep 30 & echo $! > "$0"; wait`, pidFile)
	if err == nil {
		t.Fatal("a command past its deadline returned no error")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("RunCommand returned after %s", took)
	}
	raw, rerr := os.ReadFile(pidFile)
	if rerr != nil {
		t.Fatal(rerr)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	deadline := time.Now().Add(2 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL) //nolint:errcheck
			t.Fatalf("grandchild %d outlived the deadline", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
	out, err := RunCommand(context.Background(), "", "sh", "-c", "echo ok")
	if err != nil || strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("a normal command: %q %v", out, err)
	}
}
