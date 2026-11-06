//go:build darwin

package host

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// ownProcesses lists this user's processes with the identity fields the reap
// needs, straight from the kernel (kern.proc.uid), not from ps.
func ownProcesses() ([]Process, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.uid", os.Getuid())
	if err != nil {
		return nil, err
	}
	out := make([]Process, 0, len(procs))
	for _, p := range procs {
		pid := int(p.Proc.P_pid)
		if pid <= 0 {
			continue
		}
		start := fmt.Sprintf("%d.%06d", p.Proc.P_starttime.Sec, p.Proc.P_starttime.Usec)
		out = append(out, Process{PID: pid, PPID: int(p.Eproc.Ppid), PGID: int(p.Eproc.Pgid), Start: start})
	}
	return out, nil
}

// procArgsEnv returns argv and the environment of pid from kern.procargs2
// (readable for the caller's own processes). A process that has exited or
// cannot be read yields ok=false, never a guess.
func procArgsEnv(pid int) (argv, env []string, ok bool) {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil || len(raw) < 4 {
		return nil, nil, false
	}
	argc := int(binary.LittleEndian.Uint32(raw[:4]))
	rest := raw[4:]
	// exec path, then NUL padding, then argv[0..argc), then env strings.
	i := bytes.IndexByte(rest, 0)
	if i < 0 {
		return nil, nil, false
	}
	rest = rest[i:]
	for len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}
	fields := strings.Split(string(rest), "\x00")
	for n := 0; n < argc && n < len(fields); n++ {
		argv = append(argv, fields[n])
	}
	for n := argc; n < len(fields); n++ {
		if fields[n] == "" {
			continue
		}
		env = append(env, fields[n])
	}
	return argv, env, true
}

// hasToken reports whether pid carries FAB_RUN=token in its environment.
func hasToken(pid int, token string) (command string, ok bool) {
	argv, env, found := procArgsEnv(pid)
	if !found {
		return "", false
	}
	needle := "FAB_RUN=" + token
	for _, e := range env {
		if e == needle {
			return strings.Join(argv, " "), true
		}
	}
	return "", false
}
