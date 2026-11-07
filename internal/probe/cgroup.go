// Package probe reads a runner back by attempt id (red-team G1) without
// starting or signalling anything.
package probe

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"fabric/internal/domain"
)

// Cgroup reads a runner back by attempt id from the cgroup tree
// (red-team G1). Each run is a transient user unit of the worker account,
// collected on exit (--collect), so its cgroup directory exists exactly
// while the unit does. It only reads /sys; it starts and signals nothing.
type Cgroup struct {
	Root string // the cgroup v2 mount, /sys/fs/cgroup
	UID  int    // the worker account's uid
}

// RunUnit is the unit name internal/host gives a run (unitName there); kept
// in step by TestRunUnitName.
func RunUnit(run domain.AttemptID) string {
	return "fab-" + strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return '-'
	}, string(run)) + ".service"
}

// JobAlive reads a host job's unit (StartJob names it fab-job-<id>).
func (p Cgroup) JobAlive(job string) (alive, known bool) {
	return p.unitAlive("fab-job-" + job + ".service")
}

// Alive: known is false unless the worker's app.slice is readable, so a
// missing or unfamiliar tree never reads as "gone".
func (p Cgroup) Alive(run domain.AttemptID) (alive, known bool) {
	return p.unitAlive(RunUnit(run))
}

func (p Cgroup) unitAlive(unit string) (alive, known bool) {
	slice := filepath.Join(p.Root, "user.slice", fmt.Sprintf("user-%d.slice", p.UID), fmt.Sprintf("user@%d.service", p.UID), "app.slice")
	if fi, err := os.Stat(slice); err != nil || !fi.IsDir() {
		return false, false
	}
	_, err := os.Stat(filepath.Join(slice, unit))
	switch {
	case err == nil:
		return true, true
	case errors.Is(err, fs.ErrNotExist):
		return false, true
	default:
		return false, false
	}
}
