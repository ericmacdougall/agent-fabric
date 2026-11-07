package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"fabric/internal/domain"
	"fabric/internal/sched"
)

// cgroupProber reads a runner back by attempt id from the cgroup tree
// (red-team G1). Each run is a transient user unit of the worker account,
// collected on exit (--collect), so its cgroup directory exists exactly
// while the unit does. It only reads /sys; it starts and signals nothing.
type cgroupProber struct {
	root string // the cgroup v2 mount, /sys/fs/cgroup
	uid  int    // the worker account's uid
}

// runUnit is the unit name internal/host gives a run (unitName there); kept
// in step by TestRunUnitName.
func runUnit(run domain.AttemptID) string {
	return "fab-" + strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return '-'
	}, string(run)) + ".service"
}

// Alive: known is false unless the worker's app.slice is readable, so a
// missing or unfamiliar tree never reads as "gone".
func (p cgroupProber) Alive(run domain.AttemptID) (alive, known bool) {
	slice := filepath.Join(p.root, "user.slice", fmt.Sprintf("user-%d.slice", p.uid), fmt.Sprintf("user@%d.service", p.uid), "app.slice")
	if fi, err := os.Stat(slice); err != nil || !fi.IsDir() {
		return false, false
	}
	_, err := os.Stat(filepath.Join(slice, runUnit(run)))
	switch {
	case err == nil:
		return true, true
	case errors.Is(err, fs.ErrNotExist):
		return false, true
	default:
		return false, false
	}
}

// runnerProber builds the readback for the worker account; ok is false
// when the uid cannot be resolved or the host has no cgroup v2 user tree.
func runnerProber(workerUser string) (sched.Prober, bool) {
	uid := os.Getuid()
	if workerUser != "" {
		u, err := user.Lookup(workerUser)
		if err != nil {
			return nil, false
		}
		n, err := strconv.Atoi(u.Uid)
		if err != nil {
			return nil, false
		}
		uid = n
	}
	if _, err := os.Stat("/sys/fs/cgroup/user.slice"); err != nil {
		return nil, false
	}
	return cgroupProber{root: "/sys/fs/cgroup", uid: uid}, true
}
