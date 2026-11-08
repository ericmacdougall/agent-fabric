//go:build unix

package codex

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
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

	mu     sync.Mutex
	reaped bool
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
