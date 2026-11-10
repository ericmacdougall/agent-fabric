//go:build unix

package main

import (
	"context"
	"errors"
	"io"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// ingestInput owns only its CLOEXEC duplicate. File status flags are shared
// with stdin, so parsing is exclusive and Close restores them before DB access.
// Poll/read stay synchronous: cancellation never leaves a reader goroutine.
type ingestInput struct {
	ctx   context.Context
	fd    int
	flags int
}

func newIngestInput(ctx context.Context, input *os.File) (*ingestInput, error) {
	raw, err := input.SyscallConn()
	if err != nil {
		return nil, err
	}
	r := &ingestInput{ctx: ctx, fd: -1}
	var setupErr error
	controlErr := raw.Control(func(fd uintptr) {
		r.flags, setupErr = unix.FcntlInt(fd, unix.F_GETFL, 0)
		if setupErr != nil {
			return
		}
		r.fd, setupErr = unix.FcntlInt(fd, unix.F_DUPFD_CLOEXEC, 0)
		if setupErr == nil {
			setupErr = unix.SetNonblock(r.fd, true)
		}
	})
	if err := errors.Join(controlErr, setupErr); err != nil {
		return nil, errors.Join(err, r.Close())
	}
	return r, nil
}

func (r *ingestInput) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
		if r.fd < 0 {
			return 0, os.ErrClosed
		}
		wait := 100 * time.Millisecond
		if deadline, ok := r.ctx.Deadline(); ok {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return 0, context.DeadlineExceeded
			}
			if remaining < wait {
				wait = remaining
			}
		}
		fds := []unix.PollFd{{Fd: int32(r.fd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, int((wait+time.Millisecond-1)/time.Millisecond))
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if n == 0 {
			continue
		}
		if fds[0].Revents&unix.POLLNVAL != 0 {
			return 0, os.ErrClosed
		}
		if fds[0].Revents&unix.POLLERR != 0 && fds[0].Revents&(unix.POLLIN|unix.POLLHUP) == 0 {
			return 0, errors.New("ingest stdin poll error")
		}
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
		// POLLHUP may accompany buffered bytes; read/drain before reporting EOF.
		n, err = unix.Read(r.fd, p)
		if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
			continue
		}
		if err != nil {
			return n, err
		}
		if n == 0 {
			return 0, io.EOF
		}
		return n, nil
	}
}

func (r *ingestInput) Close() error {
	if r.fd < 0 {
		return nil
	}
	_, restoreErr := unix.FcntlInt(uintptr(r.fd), unix.F_SETFL, r.flags)
	closeErr := unix.Close(r.fd)
	r.fd = -1
	return errors.Join(restoreErr, closeErr)
}
