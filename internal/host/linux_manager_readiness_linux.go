//go:build linux

package host

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type linuxUnitState struct {
	load         string
	active       string
	controlGroup string
}

func parseLinuxUnitState(out []byte) linuxUnitState {
	var state linuxUnitState
	for _, line := range strings.Split(string(out), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "LoadState":
			state.load = value
		case "ActiveState":
			state.active = value
		case "ControlGroup":
			state.controlGroup = value
		}
	}
	return state
}

func (l *LinuxManager) linuxUnitState(unit string) (linuxUnitState, error) {
	out, err := l.managerOutput("systemctl", "--user", "show", unit,
		"-p", "LoadState", "-p", "ActiveState", "-p", "ControlGroup")
	if err != nil {
		return linuxUnitState{}, fmt.Errorf("manager show: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return parseLinuxUnitState(out), nil
}

const linuxReadyContents = "ready\n"

// readLinuxReadyMarker reads the marker the confined launcher creates. The
// worker can rename or replace entries in run/jobs, so the controller never
// follows a link, never blocks on a FIFO or device, and reads a bounded
// amount from a regular single-link file owned by the worker uid
// (goodqa#15235 S5b red-team L1).
func readLinuxReadyMarker(path string, workerUID int) (bool, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return false, nil
		}
		return false, fmt.Errorf("readiness marker: %w", err)
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return false, fmt.Errorf("readiness marker stat: %w", err)
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok || !st.Mode().IsRegular() || sys.Nlink != 1 || int(sys.Uid) != workerUID || st.Size() != int64(len(linuxReadyContents)) {
		return false, errors.New("readiness marker is not a single-link regular file of the expected owner and size")
	}
	b, err := io.ReadAll(io.LimitReader(f, int64(len(linuxReadyContents))+1))
	if err != nil {
		return false, fmt.Errorf("readiness marker read: %w", err)
	}
	if string(b) != linuxReadyContents {
		return false, fmt.Errorf("readiness marker has invalid contents")
	}
	return true, nil
}

// waitUnitReady requires a trusted marker from the confined launcher. Running
// units must additionally be manager-active with a live exact cgroup. If the
// preserved launcher wait has already finished, the marker is durable proof
// that even /bin/true ran in the right cgroup and is not a readiness failure.
func (l *LinuxManager) waitUnitReady(h Handle, readyFile string, workerUID int) error {
	deadline := time.Now().Add(l.readinessTimeout())
	var last error
	marked := false // latched: once valid, the worker-writable marker is never read again
	for {
		if !marked {
			var err error
			marked, err = readLinuxReadyMarker(readyFile, workerUID)
			if err != nil {
				return fmt.Errorf("unit %s readiness: %w", h.Unit, err)
			}
		}
		if marked {
			l.mu.Lock()
			launcher := l.launcher
			l.mu.Unlock()
			if launcher != nil && launcher.finished() {
				if _, err := launcher.result(); err != nil {
					return fmt.Errorf("unit %s launcher wait: %w", h.Unit, err)
				}
				return nil
			}
			state, err := l.linuxUnitState(h.Unit)
			if err != nil {
				last = err
			} else if state.active != "active" {
				last = fmt.Errorf("manager ActiveState=%q", state.active)
			} else if state.controlGroup == "" || filepath.Join("/sys/fs/cgroup", state.controlGroup) != h.CGroup {
				last = fmt.Errorf("manager ControlGroup=%q does not match %s", state.controlGroup, h.CGroup)
			} else {
				ready, err := cgroupHasLiveProcess(h.CGroup)
				if err != nil {
					return fmt.Errorf("unit %s readiness: %w", h.Unit, err)
				}
				if ready {
					return nil
				}
				last = errors.New("unit cgroup has no readable live process")
			}
		} else {
			last = errors.New("confined launcher readiness marker is absent")
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("unit %s did not become ready: %w", h.Unit, last)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// stopAndVerifyUnit runs only after the launcher process group is reaped, so
// no new submission can arrive after the manager stop. An absent/inactive unit
// with no control group is clean; active units require a bounded stop followed
// by another manager observation.
func (l *LinuxManager) stopAndVerifyUnit(h Handle) error {
	deadline := time.Now().Add(l.managerTimeout())
	var last error
	for {
		state, err := l.linuxUnitState(h.Unit)
		if err != nil {
			last = err
		} else {
			if state.controlGroup == "" && (state.active == "" || state.active == "inactive" || state.active == "failed") {
				return nil
			}
			out, stopErr := l.managerOutput("systemctl", "--user", "stop", h.Unit)
			if stopErr != nil {
				last = fmt.Errorf("manager stop: %w: %s", stopErr, strings.TrimSpace(string(out)))
			} else {
				state, err = l.linuxUnitState(h.Unit)
				if err != nil {
					last = err
				} else if state.controlGroup == "" && (state.active == "inactive" || state.active == "failed") {
					return nil
				} else {
					last = fmt.Errorf("unit remains %s with ControlGroup=%q", state.active, state.controlGroup)
				}
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("unit %s did not reach verified stopped state: %w", h.Unit, last)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func cgroupHasLiveProcess(path string) (bool, error) {
	b, err := os.ReadFile(filepath.Join(path, "cgroup.procs"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("cgroup.procs: %w", err)
	}
	for _, field := range strings.Fields(string(b)) {
		pid, err := strconv.Atoi(field)
		if err != nil {
			return false, fmt.Errorf("cgroup.procs contains invalid pid %q", field)
		}
		_, exists, err := mgrProcIdentity(pid)
		if err != nil {
			return false, err
		}
		if exists {
			return true, nil
		}
	}
	return false, nil
}
