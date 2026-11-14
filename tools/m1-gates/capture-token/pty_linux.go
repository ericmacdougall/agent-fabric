package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

// runOnPty runs bin on a new 200x50 pty, feeding every output byte to feed
// and relaying in (the person's terminal, for a pasted code) to the pty.
func runOnPty(ctx context.Context, bin string, args, env []string, in io.Reader, feed func([]byte)) error {
	mfd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	master := os.NewFile(uintptr(mfd), "ptmx")
	defer master.Close()
	if err := unix.IoctlSetPointerInt(mfd, unix.TIOCSPTLCK, 0); err != nil {
		return err
	}
	n, err := unix.IoctlGetUint32(mfd, unix.TIOCGPTN)
	if err != nil {
		return err
	}
	if err := unix.IoctlSetWinsize(mfd, unix.TIOCSWINSZ, &unix.Winsize{Row: 50, Col: 200}); err != nil {
		return err
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		slave.Close()
		return err
	}
	slave.Close()
	if in != nil {
		go io.Copy(master, in) //nolint:errcheck // ends with the process
	}
	buf := make([]byte, 4096)
	for {
		k, rerr := master.Read(buf)
		if k > 0 {
			feed(buf[:k])
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) || errors.Is(rerr, syscall.EIO) {
				break
			}
			cmd.Process.Kill()
			cmd.Wait()
			return rerr
		}
	}
	return cmd.Wait()
}
