package tools_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fabric/internal/tools"
)

// goodqa#15283 red-team K4: a symlink swapped in at the recorded path may
// point at a name holding control or ANSI bytes. The refusal quotes it, so
// those bytes never reach a tool result or a log raw.
func TestWorktreeMovedMessageQuotesUntrustedPath(t *testing.T) {
	f := newFixture(t)
	pin, err := tools.PinWorktree(f.wt)
	if err != nil {
		t.Fatal(err)
	}
	esc := string([]byte{0x1b})
	evil := filepath.Join(f.outside, "evil"+esc+"[31mred"+string([]byte{0x07}))
	if err := os.Mkdir(evil, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(f.wt, f.wt+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(evil, f.wt); err != nil {
		t.Fatal(err)
	}
	err = pin.Check()
	if !errors.Is(err, tools.ErrWorktreeMoved) {
		t.Fatalf("Check: %v, want ErrWorktreeMoved", err)
	}
	msg := err.Error()
	if strings.ContainsAny(msg, esc+string([]byte{0x07})) {
		t.Fatalf("refusal carries raw control bytes: %q", msg)
	}
	if !strings.Contains(msg, `\x1b[31mred\a`) {
		t.Fatalf("refusal %q does not show the escaped target", msg)
	}
	// A dangling link: the EvalSymlinks error (which embeds the path) is
	// quoted too.
	if err := os.Remove(evil); err != nil {
		t.Fatal(err)
	}
	err = pin.Check()
	if !errors.Is(err, tools.ErrWorktreeMoved) || strings.ContainsAny(err.Error(), esc+string([]byte{0x07})) {
		t.Fatalf("dangling refusal %q: want ErrWorktreeMoved with no raw control bytes", err)
	}
}
