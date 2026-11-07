// Package fakehost is an unconfined stand-in for a host.Host, for tests and
// for building the native tools before the Slice 0 sandbox lands. It gives
// children a scrubbed environment and the worktree as cwd but NO
// containment: it never advertises a boundary, and a native mission refuses
// to launch on it (tools.RequireBoundary).
package fakehost

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"fabric/internal/domain"
	"fabric/internal/host"
)

type Host struct {
	mu   sync.Mutex
	cmds map[domain.AttemptID]*exec.Cmd
	// Starts records every launch (argv and env) for assertions.
	Starts []Start
}

type Start struct {
	Argv []string
	Env  []string
}

func New() *Host { return &Host{cmds: map[domain.AttemptID]*exec.Cmd{}} }

func (h *Host) Start(g host.Grant, argv []string, env []string, stdout, stderr *os.File) (host.Handle, error) {
	if err := os.MkdirAll(filepath.Join(g.RunDir, "tmp"), 0o700); err != nil {
		return host.Handle{}, err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = g.Worktree
	cmd.Env = append([]string{"PATH=/usr/bin:/bin", "HOME=" + g.RunDir, "TMPDIR=" + filepath.Join(g.RunDir, "tmp"), "LANG=C"}, env...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return host.Handle{}, err
	}
	h.mu.Lock()
	h.cmds[g.Run] = cmd
	h.Starts = append(h.Starts, Start{Argv: argv, Env: cmd.Env})
	h.mu.Unlock()
	return host.Handle{Run: g.Run, Token: g.Token, PID: cmd.Process.Pid, PGID: cmd.Process.Pid}, nil
}

func (h *Host) Wait(hd host.Handle) (int, error) {
	h.mu.Lock()
	cmd := h.cmds[hd.Run]
	h.mu.Unlock()
	err := cmd.Wait()
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), nil
	}
	return 0, err
}

func (h *Host) Stop(hd host.Handle, reason string, grace time.Duration) (host.StopReceipt, error) {
	rc := host.StopReceipt{Run: hd.Run, Reason: reason, StartedAt: time.Now()}
	syscall.Kill(-hd.PGID, syscall.SIGKILL) //nolint:errcheck
	rc.Signalled = []int{hd.PID}
	rc.Verified = true
	rc.FinishedAt = time.Now()
	return rc, nil
}
