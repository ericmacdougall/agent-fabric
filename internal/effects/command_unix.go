//go:build unix

package effects

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// commandWaitDelay is how long RunCommand waits for output pipes after the
// process group is stopped.
const commandWaitDelay = 2 * time.Second

// RunCommand runs a command for a process-based executor (review W2 on
// cbebbd6: AC-12's git push). The command gets its own process group, and at
// the context deadline the whole group is stopped with SIGKILL, so no child
// or grandchild can keep writing to the remote after the broker has given
// up; the Executor deadline contract holds. Output is combined stdout and
// stderr.
func RunCommand(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	return RunCommandEnv(ctx, dir, nil, name, args...)
}

// RunCommandEnv is RunCommand with an explicit environment (nil inherits
// the process environment).
func RunCommandEnv(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return stopGroup(cmd.Process.Pid) }
	cmd.WaitDelay = commandWaitDelay
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	// No group stop after a normal exit: once every member has gone, the
	// group id may be reused by an unrelated process (review nit on 6717adc).
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil && ctx.Err() != nil && !errors.Is(err, ctx.Err()) {
		err = errors.Join(ctx.Err(), err)
	}
	return out.Bytes(), err
}

// stopGroup sends SIGKILL to the process group led by pid.
func stopGroup(pid int) error { return syscall.Kill(-pid, syscall.SIGKILL) }
