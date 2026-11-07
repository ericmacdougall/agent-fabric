package main

import (
	"os"
	"os/user"
	"strconv"

	"fabric/internal/probe"
	"fabric/internal/sched"
)

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
	return probe.Cgroup{Root: "/sys/fs/cgroup", UID: uid}, true
}
