//go:build linux

package main

import (
	"fmt"
	"syscall"

	"fabric/internal/host"
	"fabric/internal/sched"
)

func detachAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }

func launcherForThisHost() (sched.Launcher, error) {
	p, err := profileForThisHost()
	if err != nil {
		return nil, err
	}
	// Both Linux profiles launch: the worker manager is the one a real
	// provider needs (goodqa#15976). An unchecked *host.Linux assertion
	// panicked whenever FABRIC_LINUX_WORKER_MANAGER=1 selected it.
	switch l := p.(type) {
	case *host.Linux:
		return l, nil
	case *host.LinuxManager:
		return l, nil
	}
	return nil, fmt.Errorf("profile %T cannot launch workers", p)
}
