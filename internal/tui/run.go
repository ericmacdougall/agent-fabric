//go:build linux || darwin

package tui

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Terminal control written by Run itself; never built from data.
const (
	altScreenOn  = "\x1b[?1049h\x1b[?25l"
	altScreenOff = "\x1b[?25h\x1b[?1049l"
	home         = "\x1b[H"
	clearEOL     = "\x1b[K"
)

// ErrNotTerminal means stdin or stdout is not a terminal.
var ErrNotTerminal = errors.New("fab tui needs a terminal on stdin and stdout")

// Run draws the session until q, Ctrl-C, SIGINT, SIGTERM or SIGHUP. It
// puts the terminal in raw mode on the alternate screen and restores both
// on every exit path. Refreshes run in the background, one at a time, every
// interval and right after a drill-down opens; a slow refresh never blocks
// keys or redraws.
func Run(ctx context.Context, s *Session, in, out *os.File, interval time.Duration, o Opts) error {
	inFd, outFd := int(in.Fd()), int(out.Fd())
	old, err := unix.IoctlGetTermios(inFd, ioctlGetTermios)
	if err != nil {
		return ErrNotTerminal
	}
	if _, err := unix.IoctlGetWinsize(outFd, unix.TIOCGWINSZ); err != nil {
		return ErrNotTerminal
	}
	raw := *old
	raw.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	raw.Oflag &^= unix.OPOST
	raw.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	raw.Cflag &^= unix.CSIZE | unix.PARENB
	raw.Cflag |= unix.CS8
	raw.Cc[unix.VMIN], raw.Cc[unix.VTIME] = 1, 0
	if err := unix.IoctlSetTermios(inFd, ioctlSetTermios, &raw); err != nil {
		return ErrNotTerminal
	}
	w := bufio.NewWriter(out)
	defer func() {
		w.WriteString(altScreenOff)                      //nolint:errcheck
		w.Flush()                                        //nolint:errcheck
		unix.IoctlSetTermios(inFd, ioctlSetTermios, old) //nolint:errcheck
	}()
	w.WriteString(altScreenOn) //nolint:errcheck

	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGWINCH, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)

	keys := make(chan []byte, 16)
	go func() { // exits with the process; a blocked read cannot be cancelled portably
		buf := make([]byte, 256)
		for {
			n, err := in.Read(buf)
			if err != nil {
				close(keys)
				return
			}
			b := make([]byte, n)
			copy(b, buf[:n])
			keys <- b
		}
	}()

	type result struct {
		d   *Data
		det *Detail
		err error
		id  string
	}
	results := make(chan result, 1)
	busy := false
	start := func() {
		if busy {
			return
		}
		busy = true
		id := s.M.DetailID
		go func() {
			d, det, err := s.Src.Fetch(ctx, id)
			results <- result{d, det, err, id}
		}()
	}
	draw := func() {
		ws, err := unix.IoctlGetWinsize(outFd, unix.TIOCGWINSZ)
		cols, rows := 80, 24
		if err == nil && ws.Col > 0 && ws.Row > 0 {
			cols, rows = int(ws.Col), int(ws.Row)
		}
		lines := Render(&s.M, cols, rows, time.Now(), o)
		var b strings.Builder
		b.WriteString(home)
		for i, l := range lines {
			b.WriteString(l)
			b.WriteString(clearEOL)
			if i < len(lines)-1 {
				b.WriteString("\r\n")
			}
		}
		w.WriteString(b.String()) //nolint:errcheck
		w.Flush()                 //nolint:errcheck
	}

	tick := time.NewTicker(interval)
	defer tick.Stop()
	start()
	draw()
	for !s.M.Quit {
		select {
		case <-ctx.Done():
			return nil
		case sig := <-sigs:
			if sig != syscall.SIGWINCH {
				return nil
			}
		case b, ok := <-keys:
			if !ok {
				return nil
			}
			for _, k := range ParseKeys(b) {
				if s.M.Handle(k, o.KeysOn).Refresh {
					start()
				}
			}
		case r := <-results:
			busy = false
			if r.id == s.M.DetailID { // a result for a view already left is still fresh list data
				s.M.Apply(r.d, r.det, r.err, time.Now())
			} else {
				s.M.Apply(r.d, nil, r.err, time.Now())
				start()
			}
			s.M.clamp()
		case <-tick.C:
			start()
		}
		draw()
	}
	return nil
}
