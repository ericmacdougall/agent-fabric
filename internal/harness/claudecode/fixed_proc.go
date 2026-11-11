package claudecode

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"

	"fabric/internal/harness"
	"fabric/internal/harness/proc"
)

// FixedLauncher runs the pinned, externally sandboxed command with literal
// argv/environment. It never signals a process group after a leader is reaped.
// Timeout/cancellation kills only the owned native leader; required hooks may
// have independent sessions, so callers retain evidence and report uncertainty.
func FixedLauncher(command []string) Launcher {
	return func(ctx context.Context, spec harness.StartSpec) (Proc, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(command) == 0 || spec.Dir == "" || spec.Env == nil {
			return nil, errors.New("claudecode: fixed command unavailable")
		}
		cmd := exec.Command(command[0], command[1:]...)
		cmd.Dir = spec.Dir
		cmd.Env = proc.EnvList(spec.Env)
		in, err := cmd.StdinPipe()
		if err != nil {
			return nil, err
		}
		outR, outW, err := os.Pipe()
		if err != nil {
			in.Close()
			return nil, err
		}
		errR, errW, err := os.Pipe()
		if err != nil {
			in.Close()
			outR.Close()
			outW.Close()
			return nil, err
		}
		cmd.Stdout = outW
		cmd.Stderr = errW
		if err := cmd.Start(); err != nil {
			in.Close()
			outR.Close()
			outW.Close()
			errR.Close()
			errW.Close()
			return nil, errors.New("claudecode: fixed native start failed")
		}
		outW.Close()
		errW.Close()
		p := &fixedProc{cmd: cmd, in: in, out: outR, stderr: errR, done: make(chan struct{})}
		go func() { _, _ = io.Copy(io.Discard, errR); errR.Close() }()
		go func() {
			err := cmd.Wait()
			p.mu.Lock()
			p.code = cmd.ProcessState.ExitCode()
			p.err = err
			p.mu.Unlock()
			close(p.done)
		}()
		return p, nil
	}
}

type fixedProc struct {
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    *os.File
	stderr *os.File
	done   chan struct{}
	mu     sync.Mutex
	code   int
	err    error
}

func (p *fixedProc) Stdin() io.WriteCloser   { return p.in }
func (p *fixedProc) Stdout() io.ReadCloser   { return p.out }
func (p *fixedProc) Stderr() string          { return "" }
func (p *fixedProc) Exited() <-chan struct{} { return p.done }
func (p *fixedProc) Wait() (int, error) {
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.code, p.err
}
func (p *fixedProc) Kill() error {
	// os.Process serializes Signal with Wait; never send a numeric PGID signal.
	err := p.cmd.Process.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}
