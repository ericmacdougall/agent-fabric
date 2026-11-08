package claudecode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"sync"
	"syscall"

	"fabric/internal/harness"
)

// Proc is one running Claude Code process speaking stream-json on stdio.
type Proc interface {
	Stdin() io.WriteCloser
	Stdout() io.Reader
	// Stderr returns up to the last few KiB of stderr seen so far.
	Stderr() string
	// Wait blocks until the process exits and returns its exit code (-1 when
	// it was killed by a signal).
	Wait() (int, error)
	// Kill ends the process and its children.
	Kill() error
}

// Launcher starts a process for a session.
type Launcher func(ctx context.Context, spec harness.StartSpec) (Proc, error)

// ExecLauncher runs the claude binary directly as a child of this process.
// It applies no sandbox: production use must go through a host profile
// (internal/host), which supplies its own Launcher. What it does guarantee is
// the process environment: exactly spec.Env, never the caller's, and nothing
// secret in argv.
func ExecLauncher(bin string, extraArgs ...string) Launcher {
	return func(ctx context.Context, spec harness.StartSpec) (Proc, error) {
		args := []string{"--print", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose"}
		if spec.Model != "" {
			args = append(args, "--model", spec.Model)
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
	if out == nil {
		out = []string{} // non-nil: an empty Env must not mean "inherit"
	}
	return out
}

// tail keeps the last n bytes written to it.
type tail struct {
	mu  sync.Mutex
	buf []byte
	n   int
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.n {
		t.buf = t.buf[len(t.buf)-t.n:]
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(bytes.ToValidUTF8(t.buf, []byte("?")))
}

type cmdProc struct {
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    io.Reader
	stderr *tail
	once   sync.Once
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
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	p := &cmdProc{cmd: cmd, in: in, out: out, stderr: &tail{n: 4096}}
	cmd.Stderr = p.stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("claudecode: start: %w", err)
	}
	return p, nil
}

func (p *cmdProc) Stdin() io.WriteCloser { return p.in }
func (p *cmdProc) Stdout() io.Reader     { return p.out }
func (p *cmdProc) Stderr() string        { return p.stderr.String() }

// Wait must be called after the caller has read stdout to EOF: cmd.Wait
// closes the pipe, and a read still in flight would lose output.
func (p *cmdProc) Wait() (int, error) {
	p.once.Do(func() {
		err := p.cmd.Wait()
		var ee *exec.ExitError
		switch {
		case err == nil:
		case errors.As(err, &ee):
			p.code = ee.ExitCode() // -1 when signalled
		default:
			p.code, p.err = -1, err
		}
	})
	return p.code, p.err
}

func (p *cmdProc) Kill() error {
	if p.cmd.Process == nil {
		return nil
	}
	// Setpgid made the child a group leader: signal the whole group.
	if err := syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}
