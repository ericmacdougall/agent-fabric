//go:build linux

package host

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	defaultLinuxManagerTimeout   = 2 * time.Second
	defaultLinuxReadinessTimeout = 5 * time.Second
	defaultLinuxReapTimeout      = 2 * time.Second
)

type linuxLauncher struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

func newLinuxLauncher(cmd *exec.Cmd, cancel context.CancelFunc) *linuxLauncher {
	launcher := &linuxLauncher{cmd: cmd, cancel: cancel, done: make(chan struct{})}
	go func() {
		launcher.err = cmd.Wait()
		close(launcher.done)
	}()
	return launcher
}

func (l *linuxLauncher) finished() bool {
	select {
	case <-l.done:
		return true
	default:
		return false
	}
}

func (l *linuxLauncher) result() (int, error) {
	<-l.done
	if l.err == nil {
		return 0, nil
	}
	var exit *exec.ExitError
	if errors.As(l.err, &exit) {
		return exit.ExitCode(), nil
	}
	return -1, l.err
}

// reapLinuxLauncher cancels the entire launcher/sudo process group and waits
// for the single Wait goroutine to reap it. A worker unit lives in a separate
// cgroup and is handled separately by Stop.
func reapLinuxLauncher(launcher *linuxLauncher, timeout time.Duration) error {
	if launcher == nil || launcher.finished() {
		return nil
	}
	launcher.cancel()
	if launcher.cmd.Process != nil {
		_ = syscall.Kill(-launcher.cmd.Process.Pid, syscall.SIGKILL)
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-launcher.done:
		return nil
	case <-timer.C:
		return fmt.Errorf("launcher pid %d was not reaped within %s", launcher.cmd.Process.Pid, timeout)
	}
}

func (l *LinuxManager) managerTimeout() time.Duration {
	if l.managerCallTimeout > 0 {
		return l.managerCallTimeout
	}
	return defaultLinuxManagerTimeout
}

func (l *LinuxManager) readinessTimeout() time.Duration {
	if l.unitReadinessTimeout > 0 {
		return l.unitReadinessTimeout
	}
	return defaultLinuxReadinessTimeout
}

func (l *LinuxManager) reapTimeout() time.Duration {
	if l.launcherReapTimeout > 0 {
		return l.launcherReapTimeout
	}
	return defaultLinuxReapTimeout
}

func (l *LinuxManager) command(ctx context.Context, name string, args ...string) *exec.Cmd {
	var cmd *exec.Cmd
	if l.commandFn != nil {
		cmd = l.commandFn(ctx, name, args...)
	} else {
		cmd = exec.CommandContext(ctx, name, args...)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	cmd.WaitDelay = 250 * time.Millisecond
	return cmd
}

func (l *LinuxManager) workerUID() (int, error) {
	if l.User == "" {
		return syscall.Getuid(), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), l.managerTimeout())
	defer cancel()
	out, err := l.command(ctx, "id", "-u", l.User).Output()
	if ctx.Err() != nil {
		return 0, fmt.Errorf("worker user %q lookup: %w", l.User, ctx.Err())
	}
	if err != nil {
		return 0, fmt.Errorf("worker user %q: %w", l.User, err)
	}
	return strconv.Atoi(strings.TrimSpace(string(out)))
}

// runAsWorker runs a bounded short command as the worker. Long-lived
// launchers use asWorkerContext with an explicitly cancelled lifetime
// instead. (The Kimi version discarded the cancel function; go vet
// lostcancel, goodqa#15235 S5b.)
func (l *LinuxManager) runAsWorker(tool string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), l.managerTimeout())
	defer cancel()
	return l.asWorkerContext(ctx, tool, args...).Run()
}

func (l *LinuxManager) asWorkerContext(ctx context.Context, tool string, args ...string) *exec.Cmd {
	if l.User == "" {
		return l.command(ctx, tool, args...)
	}
	uid, err := l.workerUID()
	if err != nil {
		// A failed lookup must not become uid 0 (red-team L5): the command
		// fails at Start/Run with this error and nothing is executed.
		cmd := l.command(ctx, "sudo")
		cmd.Err = fmt.Errorf("worker uid lookup: %w", err)
		return cmd
	}
	full := append([]string{"-n", "-u", l.User, "env",
		fmt.Sprintf("XDG_RUNTIME_DIR=/run/user/%d", uid),
		fmt.Sprintf("DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/%d/bus", uid), tool}, args...)
	return l.command(ctx, "sudo", full...)
}

func (l *LinuxManager) managerOutput(tool string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), l.managerTimeout())
	defer cancel()
	out, err := l.asWorkerContext(ctx, tool, args...).CombinedOutput()
	if ctx.Err() != nil {
		return out, fmt.Errorf("%s deadline: %w", tool, ctx.Err())
	}
	return out, err
}
