package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// Failure classes: the connection and operator-intervention classes (and
// a refused or unreachable server) are "disconnected"; any other server
// answer, such as a lock or statement timeout, is "stale".
func TestClassify(t *testing.T) {
	for _, c := range []struct {
		err  error
		lost bool
		code string
	}{
		{&pgconn.PgError{Code: "57P01"}, true, "57P01"},
		{&pgconn.PgError{Code: "08006"}, true, "08006"},
		{&pgconn.PgError{Code: "55000"}, true, "55000"},
		{&pgconn.PgError{Code: "3D000"}, true, "3D000"},
		{&pgconn.PgError{Code: "55P03"}, false, "55P03"},
		{&pgconn.PgError{Code: "57014"}, false, "57014"},
		{fmt.Errorf("begin: %w", &pgconn.PgError{Code: "25006"}), false, "25006"},
		{errors.New("dial tcp: connection refused"), true, ""},
		// Review F2 on ff48df1: the client deadline means the server did not
		// answer in time, not that the connection is gone.
		{fmt.Errorf("begin: %w", context.DeadlineExceeded), false, ""},
	} {
		f := classify(c.err)
		if f.Disconnected != c.lost || f.Code != c.code {
			t.Errorf("classify(%v) = %+v", c.err, f)
		}
		if strings.Contains(f.String(), "dial") {
			t.Errorf("failure text echoes the error: %q", f.String())
		}
	}
}

// NEG (sanitizer): control, escape, C1, bidi and invalid bytes never
// survive Clean, and the result fits its width.
func TestClean(t *testing.T) {
	cases := []struct {
		name, in string
		max      int
		want     string
	}{
		{"plain", "goodqa#1 build", 0, "goodqa#1 build"},
		{"csi", "a\x1b[31mred\x1b[0m", 0, "a [31mred [0m"},
		{"osc8 and bel", "\x1b]8;;http://x\x07link\x1b]8;;\x07", 0, " ]8;;http://x link ]8;; "},
		{"osc52", "\x1b]52;c;ZXZpbA==\x07", 0, " ]52;c;ZXZpbA== "},
		{"c1 csi", "x\u009b2Jy", 0, "x 2Jy"},
		{"del and newline", "a\x7fb\r\nc\td", 0, "a b  c d"},
		{"bidi override", "abc\u202edcba\u2066x\u2069", 0, "abcdcbax"},
		{"zero width", "a\u200bb\ufeffc\u00add", 0, "abcd"},
		{"invalid utf8", "a\xffb\xc3", 0, "a?b?"},
		{"cut", "abcdefghij", 5, "abcd~"},
		{"cjk cut", "漢字漢字漢字", 5, "漢字~"},
		{"fits", "abc", 3, "abc"},
		{"tag characters", "a\U000e0041\U000e0042b\U000e007f", 0, "ab"},
		{"emoji width", "\u2705\u2b50x", 4, "\u2705~"},
	}
	for _, c := range cases {
		got := Clean(c.in, c.max)
		if got != c.want {
			t.Errorf("%s: Clean(%q, %d) = %q, want %q", c.name, c.in, c.max, got, c.want)
		}
		if c.max > 0 && Width(got) > c.max {
			t.Errorf("%s: width %d > %d", c.name, Width(got), c.max)
		}
		assertInert(t, c.name, got)
	}
	if got := Pad("漢", 4); got != "漢  " || Width(got) != 4 {
		t.Errorf("Pad = %q", got)
	}
}

// assertInert fails if s holds anything a terminal could act on: ESC, BEL
// or another C0 control, DEL, a C1 control, or a bidi/zero-width format
// character.
func assertInert(t *testing.T, what, s string) {
	t.Helper()
	for i, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) || invisible(r) {
			t.Fatalf("%s: control or format rune %U at byte %d in %q", what, r, i, s)
		}
	}
	if strings.ContainsRune(s, 0xfffd) {
		t.Fatalf("%s: replacement rune leaked in %q", what, s)
	}
}

// Review F1 on ff48df1: autowrap is off while the view is up, so a line
// that is still too wide is clipped, never wrapped onto the next row; it
// is restored on exit.
func TestAutowrapOff(t *testing.T) {
	if !strings.Contains(altScreenOn, "\x1b[?7l") || !strings.Contains(altScreenOff, "\x1b[?7h") {
		t.Fatalf("autowrap not disabled/restored: %q / %q", altScreenOn, altScreenOff)
	}
}
