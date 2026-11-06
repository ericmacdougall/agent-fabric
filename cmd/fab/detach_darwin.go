//go:build darwin

package main

import (
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
	return p.(*host.Darwin), nil
}
