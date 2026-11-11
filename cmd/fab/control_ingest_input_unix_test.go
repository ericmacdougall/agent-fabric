//go:build unix

package main

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func fileStatus(t *testing.T, f *os.File) int {
	t.Helper()
	raw, err := f.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var flags int
	var readErr error
	if err := raw.Control(func(fd uintptr) { flags, readErr = unix.FcntlInt(fd, unix.F_GETFL, 0) }); err != nil {
		t.Fatal(err)
	}
	if readErr != nil {
		t.Fatal(readErr)
	}
	return flags
}

func TestControlIngestInputCancellationAndCleanup(t *testing.T) {
	for _, deadline := range []bool{true, false} {
		name := "cancel"
		if deadline {
			name = "deadline"
		}
		t.Run(name, func(t *testing.T) {
			input, producer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer input.Close()
			defer producer.Close()
			input.Fd() // make the pipe blocking, as inherited fd0 is in the CLI child
			before := fileStatus(t, input)
			var ctx context.Context
			var cancel context.CancelFunc
			if deadline {
				ctx, cancel = context.WithTimeout(context.Background(), 25*time.Millisecond)
			} else {
				ctx, cancel = context.WithCancel(context.Background())
				timer := time.AfterFunc(25*time.Millisecond, cancel)
				defer timer.Stop()
			}
			defer cancel()
			reader, err := newIngestInput(ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close() //nolint:errcheck
			owned := reader.fd
			flags, err := unix.FcntlInt(uintptr(owned), unix.F_GETFD, 0)
			if err != nil || flags&unix.FD_CLOEXEC == 0 {
				t.Fatalf("duplicate must be close-on-exec: flags=%d err=%v", flags, err)
			}
			start := time.Now()
			_, err = reader.Read(make([]byte, 1))
			want := context.Canceled
			if deadline {
				want = context.DeadlineExceeded
			}
			if !errors.Is(err, want) || time.Since(start) > 300*time.Millisecond {
				t.Fatalf("input did not honor cancellation: err=%v elapsed=%v", err, time.Since(start))
			}
			if err := reader.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := unix.FcntlInt(uintptr(owned), unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
				t.Fatalf("owned duplicate leaked: %v", err)
			}
			if after := fileStatus(t, input); before != after {
				t.Fatalf("stdin flags changed: %d -> %d", before, after)
			}
			if _, err := producer.Write([]byte("x")); err != nil {
				t.Fatal(err)
			}
			var one [1]byte
			if n, err := input.Read(one[:]); err != nil || n != 1 || one[0] != 'x' {
				t.Fatalf("original stdin was closed or consumed: n=%d err=%v", n, err)
			}
			if _, err := reader.Read(one[:]); !errors.Is(err, want) && !errors.Is(err, os.ErrClosed) {
				t.Fatalf("read after cleanup: %v", err)
			}
		})
	}
}

func TestControlIngestInputDrainsEOF(t *testing.T) {
	input, producer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	if _, err := producer.Write([]byte("last buffered line\n")); err != nil {
		t.Fatal(err)
	}
	producer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	reader, err := newIngestInput(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close() //nolint:errcheck
	data, err := io.ReadAll(reader)
	if err != nil || string(data) != "last buffered line\n" {
		t.Fatalf("POLLHUP lost buffered data: %q err=%v", data, err)
	}
}

func TestControlIngestInputRegularFile(t *testing.T) {
	input, err := os.CreateTemp(t.TempDir(), "input")
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	if _, err := input.WriteString("regular input"); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	before := fileStatus(t, input)
	reader, err := newIngestInput(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || string(data) != "regular input" || before != fileStatus(t, input) {
		t.Fatalf("regular input/cleanup: data=%q read=%v close=%v", data, readErr, closeErr)
	}
}
