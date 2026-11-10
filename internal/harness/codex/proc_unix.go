//go:build unix

package codex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// proc is one harness process in its own process group, with explicit pipes
// so the adapter controls write deadlines and when reading stops.
type proc struct {
	cmd    *exec.Cmd
	stdin  *os.File // parent's write end
	stdout *os.File // parent's read end

	mu      sync.Mutex
	reaped  bool
	waitErr error
}

// cappedWriter keeps diagnostics out of the event stream (they can hold local
// paths and account details) while still draining the pipe. It records only
// how much was written.
type cappedWriter struct {
	mu sync.Mutex
	n  int64
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.n += int64(len(p))
	c.mu.Unlock()
	return len(p), nil
}

// spawn starts argv with exactly env as its environment. The caller's own
// environment is never inherited.
func spawn(argv, env []string, dir string, stderr io.Writer) (*proc, error) {
	if len(argv) == 0 {
		return nil, errors.New("codex: empty command")
	}
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return nil, err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append([]string{}, env...) // non-nil: an empty list means an empty environment
	cmd.Dir = dir
	cmd.Stdin = inR
	cmd.Stdout = outW
	cmd.Stderr = stderr
	cmd.WaitDelay = 2 * time.Second
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		return nil, fmt.Errorf("codex: start %s: %w", argv[0], err)
	}
	inR.Close()
	outW.Close()
	return &proc{cmd: cmd, stdin: inW, stdout: outR}, nil
}

// write sends one line to the process, giving up after d.
func (p *proc) write(b []byte, d time.Duration) error {
	_ = p.stdin.SetWriteDeadline(time.Now().Add(d))
	_, err := p.stdin.Write(b)
	return err
}

// signalGroup sends sig to the process group unless it is already reaped.
func (p *proc) signalGroup(sig syscall.Signal) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.reaped {
		return
	}
	_ = syscall.Kill(-p.cmd.Process.Pid, sig)
}

// wait reaps the process and reports its exit status; -1 means a signal.
func (p *proc) wait() (code int, err error) {
	err = p.cmd.Wait()
	p.waitErr = err
	p.mu.Lock()
	p.reaped = true
	p.mu.Unlock()
	return p.cmd.ProcessState.ExitCode(), err
}

func exitText(code int, err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ProcessState.String()
	}
	if err != nil {
		return err.Error()
	}
	return fmt.Sprintf("exit status %d", code)
}

// cleanup retains the leader (and therefore the process-group identity) until
// every signal and live-member check is complete. It does not contain setsid
// or double-fork escapes; controlled mode must exclude arbitrary process tools.
func (p *proc) cleanup(grace time.Duration) error {
	p.signalGroup(syscall.SIGTERM)
	empty := p.waitGroupEmpty(grace)
	if !empty {
		p.signalGroup(syscall.SIGKILL)
		empty = p.waitGroupEmpty(grace)
	}
	// No further group signal may happen after Wait releases the leader PID.
	_, _ = p.wait()
	if !empty {
		return errors.New("owned process group still active or unobservable")
	}
	return nil
}

func (p *proc) waitGroupEmpty(bound time.Duration) bool {
	deadline := time.Now().Add(bound)
	for {
		live, err := p.liveGroup()
		if err != nil {
			return false
		}
		if !live {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Only pid/pgid/state metadata is requested. No argv, account or environment
// enters this observation. Failure is uncertainty, never an empty group.
func (p *proc) liveGroup() (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/ps", "-axo", "pid=,pgid=,stat=")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LC_ALL=C"}
	b, err := cmd.Output()
	if err != nil {
		return false, err
	}
	group := p.cmd.Process.Pid
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) != 3 {
			continue
		}
		pg, e := strconv.Atoi(f[1])
		if e != nil {
			return false, e
		}
		if pg == group && !strings.HasPrefix(f[2], "Z") {
			return true, nil
		}
	}
	return false, nil
}

// leaderExited observes without Wait/reaping. Linux exposes zombie state in
// procfs; Darwin uses a fixed metadata-only ps request. Retaining the zombie
// pins the PID/group even when a descendant keeps stdout open.
func (p *proc) leaderExited() (bool, error) {
	if runtime.GOOS == "linux" {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", p.cmd.Process.Pid))
		if err != nil {
			return false, err
		}
		i := strings.LastIndex(string(b), ") ")
		if i < 0 || len(b) <= i+2 {
			return false, errors.New("invalid leader state")
		}
		return b[i+2] == 'Z', nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/ps", "-o", "stat=", "-p", strconv.Itoa(p.cmd.Process.Pid))
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LC_ALL=C"}
	b, err := cmd.Output()
	if err != nil {
		return false, err
	}
	state := strings.TrimSpace(string(b))
	if state == "" {
		return false, errors.New("leader state missing")
	}
	return strings.HasPrefix(state, "Z"), nil
}
