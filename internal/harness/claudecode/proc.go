package claudecode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"syscall"

	"fabric/internal/harness"
)

// Proc is one running Claude Code process speaking stream-json on stdio.
type Proc interface {
	Stdin() io.WriteCloser
	// Stdout is the read end of the child's stdout. Closing it makes a
	// blocked read return, so a descendant that kept the pipe open cannot
	// hold the session forever.
	Stdout() io.ReadCloser
	// Stderr returns the last part of stderr seen so far, cut at a token
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

// Launcher starts a process for a session.
type Launcher func(ctx context.Context, spec harness.StartSpec) (Proc, error)

var modelRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:\-]{0,63}$`)

// ExecLauncher runs the claude binary directly as a child of this process.
// It applies no sandbox: production use must go through a host profile
// (internal/host), which supplies its own Launcher. What it does guarantee is
// the process environment: exactly spec.Env, never the caller's; an absolute
// working directory; a validated model; and nothing secret in argv.
func ExecLauncher(bin string, extraArgs ...string) Launcher {
	return func(ctx context.Context, spec harness.StartSpec) (Proc, error) {
		if !filepath.IsAbs(spec.Dir) {
			return nil, errors.New("claudecode: Dir must be an absolute path (an empty Dir would run the agent in the caller's directory)")
		}
		args := []string{"--print", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose"}
		if spec.Model != "" {
			if !modelRE.MatchString(spec.Model) {
				return nil, fmt.Errorf("claudecode: model %q is not an allowed model name", spec.Model)
			}
			args = append(args, "--model="+spec.Model)
		}
		args = append(args, extraArgs...)
		cmd := exec.Command(bin, args...)
		cmd.Dir = spec.Dir
		cmd.Env = envList(spec.Env)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		return startCmd(cmd)
	}
}

func envList(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out // non-nil: an empty Env must not mean "inherit"
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
// token is dropped too, so a key cut in half does not lose its recognisable
// prefix while keeping its secret remainder.
func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	b := t.buf
	if t.wrapd {
		if i := bytes.IndexAny(b, " \t\r\n"); i >= 0 {
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

func startCmd(cmd *exec.Cmd) (Proc, error) {
	if cmd.Env == nil {
		return nil, errors.New("claudecode: refusing to start with an inherited environment")
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
		return nil, fmt.Errorf("claudecode: start: %w", err)
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
		// Whatever the leader left behind in its group dies with it. The
		// group id is still ours: signal it before marking the leader reaped.
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
	// Setpgid made the child a group leader: signal the whole group.
	if err := syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}
