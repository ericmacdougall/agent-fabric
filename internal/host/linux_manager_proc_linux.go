//go:build linux

package host

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// readProcFile distinguishes a process that demonstrably disappeared from an
// observation failure. Only ENOENT/ESRCH is absence; permission, I/O and every
// other error make inventory fail closed.
func readProcFile(path string) ([]byte, bool, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		return b, true, nil
	}
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return nil, false, nil
	}
	return nil, false, fmt.Errorf("read %s: %w", path, err)
}

func procIdentityAt(root string, pid int) (Process, bool, error) {
	stat, exists, err := readProcFile(fmt.Sprintf("%s/%d/stat", root, pid))
	if err != nil || !exists {
		return Process{}, exists, err
	}
	s := string(stat)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return Process{}, true, fmt.Errorf("proc identity pid %d: malformed stat", pid)
	}
	fields := strings.Fields(s[i+1:])
	if len(fields) < 20 {
		return Process{}, true, fmt.Errorf("proc identity pid %d: short stat", pid)
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return Process{}, true, fmt.Errorf("proc identity pid %d: ppid: %w", pid, err)
	}
	pgid, err := strconv.Atoi(fields[2])
	if err != nil {
		return Process{}, true, fmt.Errorf("proc identity pid %d: pgid: %w", pid, err)
	}
	cmdline, exists, err := readProcFile(fmt.Sprintf("%s/%d/cmdline", root, pid))
	if err != nil || !exists {
		return Process{}, exists, err
	}
	return Process{
		PID: pid, PPID: ppid, PGID: pgid, Start: fields[19],
		Command: strings.TrimSpace(string(bytes.ReplaceAll(cmdline, []byte{0}, []byte{' '}))),
	}, true, nil
}

func mgrProcIdentity(pid int) (Process, bool, error) {
	return procIdentityAt("/proc", pid)
}

func procUIDAt(root string, pid int) (int, bool, error) {
	status, exists, err := readProcFile(fmt.Sprintf("%s/%d/status", root, pid))
	if err != nil || !exists {
		return 0, exists, err
	}
	for _, line := range strings.Split(string(status), "\n") {
		if !strings.HasPrefix(line, "Uid:") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "Uid:"))
		if len(fields) < 1 {
			break
		}
		uid, err := strconv.Atoi(fields[0])
		if err != nil {
			return 0, true, fmt.Errorf("proc uid pid %d: %w", pid, err)
		}
		return uid, true, nil
	}
	return 0, true, fmt.Errorf("proc uid pid %d: missing Uid record", pid)
}

func uidProcessesAt(root string, uid int, inside map[int]bool, runCGroup string) ([]Process, error) {
	managerPrefix := fmt.Sprintf("/user.slice/user-%d.slice/user@%d.service/", uid, uid)
	unitRel := strings.TrimPrefix(runCGroup, "/sys/fs/cgroup")
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read proc inventory: %w", err)
	}
	var out []Process
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || inside[pid] {
			continue
		}
		owner, exists, err := procUIDAt(root, pid)
		if err != nil {
			return nil, err
		}
		if !exists || owner != uid {
			continue
		}
		cgroup, exists, err := readProcFile(fmt.Sprintf("%s/%d/cgroup", root, pid))
		if err != nil {
			return nil, err
		}
		if !exists {
			continue
		}
		text := strings.TrimSpace(string(cgroup))
		if !strings.HasPrefix(text, "0::/") {
			return nil, fmt.Errorf("proc cgroup pid %d: unsupported or malformed record", pid)
		}
		path := strings.TrimPrefix(text, "0::")
		if !strings.HasPrefix(path, managerPrefix) || path == unitRel || strings.HasPrefix(path, unitRel+"/") {
			continue
		}
		rel := strings.TrimPrefix(path, managerPrefix)
		if workerManagerEscape(rel, unitRel) {
			return nil, fmt.Errorf("unexpected worker-manager sibling %s (pid %d); refusing inventory and stop verification", rel, pid)
		}
	}
	return out, nil
}

func mgrUidProcesses(uid int, inside map[int]bool, runCGroup string) ([]Process, error) {
	return uidProcessesAt("/proc", uid, inside, runCGroup)
}

func tokenCarriersAt(root, token string, inside map[int]bool, uid int) ([]Process, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read proc token inventory: %w", err)
	}
	needle := []byte("FAB_RUN=" + token)
	self := os.Getpid()
	var out []Process
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == self || inside[pid] {
			continue
		}
		owner, exists, err := procUIDAt(root, pid)
		if err != nil {
			return nil, err
		}
		if !exists || owner != uid {
			continue
		}
		environ, exists, err := readProcFile(fmt.Sprintf("%s/%d/environ", root, pid))
		if err != nil {
			return nil, err
		}
		if !exists {
			continue
		}
		for _, kv := range bytes.Split(environ, []byte{0}) {
			if !bytes.Equal(kv, needle) {
				continue
			}
			process, exists, err := procIdentityAt(root, pid)
			if err != nil {
				return nil, err
			}
			if exists {
				process.Escaped = true
				out = append(out, process)
			}
			break
		}
	}
	return out, nil
}

func mgrTokenCarriers(token string, inside map[int]bool, uid int) ([]Process, error) {
	return tokenCarriersAt("/proc", token, inside, uid)
}
