package claudecode

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"syscall"

	"fabric/internal/harness"
	"fabric/internal/harness/proc"
)

// Proc is one running Claude Code process speaking stream-json on stdio.
type Proc = proc.Proc

// Launcher starts a process for a session.
type Launcher func(ctx context.Context, spec harness.StartSpec) (Proc, error)

var modelRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:\-]{0,63}$`)

// startCmd and envList are kept for the package's tests.
var (
	startCmd = proc.StartCmd
	envList  = proc.EnvList
)

// ExecLauncher runs the claude binary directly as a child of this process.
// It applies no sandbox: production use must go through a host profile
// (internal/host), which supplies its own Launcher. What it does guarantee is
// the process environment: exactly spec.Env, never the caller's; an absolute
// working directory; a validated model; and nothing secret in argv.
func ExecLauncher(bin string, extraArgs ...string) Launcher {
	return func(ctx context.Context, spec harness.StartSpec) (Proc, error) {
		if !filepath.IsAbs(spec.Dir) {
			return nil, fmt.Errorf("claudecode: Dir must be an absolute path (an empty Dir would run the agent in the caller's directory)")
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
		cmd.Env = proc.EnvList(spec.Env)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		return proc.StartCmd(cmd)
	}
}
