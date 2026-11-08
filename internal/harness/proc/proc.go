// Package proc runs an agent harness as a child process for the harness
// adapters (claude-code, kimi, ...): file-backed stdio so a descendant
// holding a pipe cannot delay the leader's exit being seen, a group kill at
// leader exit, an exact environment, and a bounded stderr tail cut at a line
// boundary. It knows nothing about any harness's protocol.
package proc

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"sync"
	"syscall"
)

// Proc is one running harness process.
type Proc interface {
	Stdin() io.WriteCloser
	// Stdout is the read end of the child's stdout. Closing it makes a
	// blocked read return, so a descendant that kept the pipe open cannot
	// hold the session forever.
	Stdout() io.ReadCloser
	// Stderr returns the last part of stderr seen so far, cut at a line
	// boundary so a partial secret prefix is not left at the start.
	Stderr() string
	// Exited is closed once the leader has exited and been reaped and its
	// process group has been signalled.
	Exited() <-chan struct{}
	// Wait blocks until Exited and returns the exit code (-1 when killed by a
	// signal).
	Wait() (int, error)
	// Kill ends the process group. It is a no-op once the leader is reaped.
	Kill() error
}

// EnvList renders an allow-list as KEY=VALUE entries, sorted. The result is
// never nil: an empty allow-list must not mean "inherit".
func EnvList(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// tail keeps the last n bytes written to it.
type tail struct {
	mu    sync.Mutex
	buf   []byte
	n     int
	wrapd bool
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.n {
		t.buf = t.buf[len(t.buf)-t.n:]
		t.wrapd = true
	}
	return len(p), nil
}

// String returns the tail. When older bytes were dropped, the first partial
// line is dropped too, so a key cut in half does not lose its recognisable
// prefix while keeping its secret remainder.
func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	b := t.buf
	if t.wrapd {
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			b = b[i:]
		} else {
			b = nil
		}
	}
	return string(bytes.ToValidUTF8(b, []byte("?")))
}

const stderrTail = 16 << 10

type cmdProc struct {
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    *os.File
	stderr *tail

	mu     sync.Mutex
	reaped bool
	exited chan struct{}
	code   int
	err    error
}

// StartCmd starts cmd under the contract above. cmd.Env must be set (an
// empty, non-nil list is valid); cmd.SysProcAttr should make the child a
// process-group leader (Setpgid) so a group kill reaches its descendants.
func StartCmd(cmd *exec.Cmd) (Proc, error) {
	if cmd.Env == nil {
		return nil, errors.New("proc: refusing to start with an inherited environment")
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	or, ow, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	er, ew, err := os.Pipe()
	if err != nil {
		or.Close()
		ow.Close()
		return nil, err
	}
	// File-backed stdio: cmd.Wait does not wait on a copy goroutine, so a
	// descendant holding a pipe cannot delay the leader's exit being seen.
	cmd.Stdout, cmd.Stderr = ow, ew
	if err := cmd.Start(); err != nil {
		or.Close()
		ow.Close()
		er.Close()
		ew.Close()
		return nil, fmt.Errorf("proc: start: %w", err)
	}
	ow.Close()
	ew.Close()
	p := &cmdProc{cmd: cmd, in: in, out: or, stderr: &tail{n: stderrTail}, exited: make(chan struct{})}
	go func() {
		_, _ = io.Copy(p.stderr, er)
		er.Close()
	}()
	go func() {
		err := cmd.Wait()
		p.mu.Lock()
		var ee *exec.ExitError
		switch {
		case err == nil:
		case errors.As(err, &ee):
			p.code = ee.ExitCode() // -1 when signalled
		default:
			p.code, p.err = -1, err
		}
		// Whatever the leader left behind in its group dies with it.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		p.reaped = true
		p.mu.Unlock()
		close(p.exited)
	}()
	return p, nil
}

func (p *cmdProc) Stdin() io.WriteCloser   { return p.in }
func (p *cmdProc) Stdout() io.ReadCloser   { return p.out }
func (p *cmdProc) Stderr() string          { return p.stderr.String() }
func (p *cmdProc) Exited() <-chan struct{} { return p.exited }

func (p *cmdProc) Wait() (int, error) {
	<-p.exited
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.code, p.err
}

func (p *cmdProc) Kill() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.reaped || p.cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// ReadLines calls f for every line of r, with the line truncated to max and
// oversize set when it was longer. A line longer than max is skipped to its
// end, not allowed to grow memory.
func ReadLines(r io.Reader, max int, f func(line []byte, oversize bool)) {
	br := bufio.NewReaderSize(r, 64<<10)
	var cur []byte
	over := false
	for {
		chunk, err := br.ReadSlice('\n')
		if len(chunk) > 0 && !over {
			if len(cur)+len(chunk) > max {
				over = true
				cur = append(cur, chunk[:max-len(cur)]...)
			} else {
				cur = append(cur, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if len(chunk) > 0 || len(cur) > 0 {
			line := cur
			for len(line) > 0 && (line[len(line)-1] == '\n' || line[len(line)-1] == '\r') {
				line = line[:len(line)-1]
			}
			f(line, over)
		}
		cur, over = nil, false
		if err != nil {
			return
		}
	}
}
