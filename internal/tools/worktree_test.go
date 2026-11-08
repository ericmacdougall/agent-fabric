package tools_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"fabric/internal/tools"
)

// goodqa#15283 AC-5: the shared worktree pin refuses every entry point once
// the recorded directory moved: a parent rename with the same path recreated
// as a new directory (only dev/ino tells them apart), a symlink in its place,
// and removal.
func TestWorktreePinRefusesMovedWorktree(t *testing.T) {
	for _, how := range []string{"parent renamed, path recreated", "path replaced by a symlink", "removed"} {
		t.Run(how, func(t *testing.T) {
			f := newFixture(t)
			write(t, filepath.Join(f.wt, "notes.txt"), "alpha")
			pin, err := tools.PinWorktree(f.wt)
			if err != nil {
				t.Fatal(err)
			}
			r, err := tools.NewRead(f.wt, 1000, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			r.Pin = pin
			f.conf.Pin = pin
			g := &tools.Grep{C: f.conf, Root: f.wt, Timeout: 5 * time.Second, MaxOutput: 4000, Pin: pin}
			sh := &tools.Shell{C: f.conf, Allow: []*regexp.Regexp{regexp.MustCompile(`^echo hi$`)}, Timeout: 5 * time.Second, MaxOutput: 4000}
			ctx := context.Background()
			// Controls before the move: every entry point works.
			if err := pin.Check(); err != nil {
				t.Fatalf("control: %v", err)
			}
			if out, isErr := r.Run(ctx, input("path", "notes.txt")); isErr || out != "alpha" {
				t.Fatalf("control read: %q %v", out, isErr)
			}
			if out, isErr := g.Run(ctx, input("pattern", "alpha")); isErr || !strings.Contains(out, "notes.txt") {
				t.Fatalf("control grep: %q %v", out, isErr)
			}
			if out, isErr := sh.Run(ctx, input("command", "echo hi")); isErr || !strings.Contains(out, "hi") {
				t.Fatalf("control shell: %q %v", out, isErr)
			}
			starts := len(f.fh.Starts)

			parent := filepath.Dir(f.wt)
			moved := parent + ".moved"
			switch how {
			case "parent renamed, path recreated":
				if err := os.Rename(parent, moved); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.RemoveAll(moved) })
				for _, d := range []string{f.wt, f.run} {
					if err := os.MkdirAll(d, 0o755); err != nil {
						t.Fatal(err)
					}
				}
				write(t, filepath.Join(f.wt, "notes.txt"), "alpha planted")
			case "path replaced by a symlink":
				if err := os.Rename(f.wt, f.wt+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(f.outside, f.wt); err != nil {
					t.Fatal(err)
				}
				write(t, filepath.Join(f.outside, "notes.txt"), "alpha outside")
			case "removed":
				if err := os.RemoveAll(f.wt); err != nil {
					t.Fatal(err)
				}
			}
			if err := pin.Check(); !errors.Is(err, tools.ErrWorktreeMoved) {
				t.Fatalf("pin.Check after %s: %v, want ErrWorktreeMoved", how, err)
			}
			moved1 := tools.ErrWorktreeMoved.Error()
			if out, isErr := r.Run(ctx, input("path", "notes.txt")); !isErr || !strings.Contains(out, moved1) {
				t.Errorf("read after %s: %q, want refused with %q", how, out, moved1)
			}
			if out, isErr := g.Run(ctx, input("pattern", "alpha")); !isErr || !strings.Contains(out, moved1) {
				t.Errorf("grep after %s: %q, want refused with %q", how, out, moved1)
			}
			if out, isErr := sh.Run(ctx, input("command", "echo hi")); !isErr || !strings.Contains(out, moved1) {
				t.Errorf("shell after %s: %q, want refused with %q", how, out, moved1)
			}
			if _, err := f.conf.Run(ctx, tools.ExecSpec{Argv: []string{"true"}}); !errors.Is(err, tools.ErrWorktreeMoved) {
				t.Errorf("host start after %s: %v, want ErrWorktreeMoved", how, err)
			}
			if n := len(f.fh.Starts); n != starts {
				t.Errorf("after %s the host started %d more children, want none", how, n-starts)
			}
		})
	}
}

// A path that does not resolve to itself cannot be pinned: the caller must
// canonicalize first.
func TestPinWorktreeNeedsCanonicalDirectory(t *testing.T) {
	f := newFixture(t)
	link := filepath.Join(filepath.Dir(f.wt), "link")
	if err := os.Symlink(f.wt, link); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(f.wt, "file")
	write(t, file, "x")
	for _, p := range []string{link, file, filepath.Join(f.wt, "missing")} {
		if _, err := tools.PinWorktree(p); !errors.Is(err, tools.ErrWorktreeMoved) {
			t.Errorf("PinWorktree(%s) = %v, want refused", p, err)
		}
	}
}
