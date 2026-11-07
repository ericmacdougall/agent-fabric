// Package tui is fab tui: a read-only terminal view of the fabric's
// missions, attempts, effects and asks, refreshed on an interval.
//
// Rendering is a pure function of the model (Render), so frames are tested
// as golden text. Every string that came from the database passes through
// Clean before it reaches the terminal: a ticket or ask question is
// runner-writable, so it must never carry a control sequence.
package tui

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Clean makes s safe and bounded for one terminal cell run:
//   - C0 controls (including ESC, BEL, CR, LF, TAB), DEL and C1 controls
//     (U+0080-U+009F, which some terminals treat as CSI/OSC introducers)
//     become a space, so no escape sequence can start;
//   - bidi embeddings, overrides and isolates and zero-width characters,
//     which can reorder or hide what is shown, are dropped;
//   - invalid UTF-8 becomes '?';
//   - the result is cut to max display columns (no limit when max <= 0),
//     ending in '~' when it was cut.
func Clean(s string, max int) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		switch {
		case r == utf8.RuneError && size <= 1:
			b.WriteByte('?')
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
			b.WriteByte(' ')
		case invisible(r):
		default:
			b.WriteRune(r)
		}
	}
	return Fit(b.String(), max)
}

// invisible reports format characters that reorder or hide text.
func invisible(r rune) bool {
	switch {
	case r >= 0x200b && r <= 0x200f: // zero-width space, joiners, LRM, RLM
		return true
	case r >= 0x202a && r <= 0x202e: // bidi embeddings and overrides
		return true
	case r >= 0x2066 && r <= 0x2069: // bidi isolates
		return true
	case r == 0x061c || r == 0xfeff || r == 0x00ad: // ALM, BOM, soft hyphen
		return true
	case r >= 0xe0000 && r <= 0xe007f: // tag characters (invisible "ASCII smuggling")
		return true
	}
	return false
}

// runeWidth is a conservative display width: wide East Asian and emoji
// ranges count as 2 so a line never overflows; combining marks count as 0.
// Every symbol (category So) from U+2300 up counts as 2, because terminals
// draw many of them (U+2705, U+231A, U+26A1, U+2B50 ...) as two-cell emoji;
// over-counting only costs alignment (review F1 on ff48df1).
func runeWidth(r rune) int {
	switch {
	case r >= 0x300 && r <= 0x36f: // combining diacritics
		return 0
	case r >= 0x2300 && unicode.Is(unicode.So, r):
		return 2
	case r >= 0x1100 && r <= 0x115f, r >= 0x2e80 && r <= 0xa4cf, r >= 0xac00 && r <= 0xd7a3,
		r >= 0xf900 && r <= 0xfaff, r >= 0xfe30 && r <= 0xfe4f, r >= 0xff00 && r <= 0xff60,
		r >= 0xffe0 && r <= 0xffe6, r >= 0x1f000 && r <= 0x1faff, r >= 0x20000 && r <= 0x3fffd:
		return 2
	}
	return 1
}

// Width is the display width of s (already cleaned).
func Width(s string) int {
	n := 0
	for _, r := range s {
		n += runeWidth(r)
	}
	return n
}

// Fit cuts s to at most w display columns, ending in '~' when cut. w <= 0
// means no limit.
func Fit(s string, w int) string {
	if w <= 0 || Width(s) <= w {
		return s
	}
	var b strings.Builder
	n := 0
	for _, r := range s {
		rw := runeWidth(r)
		if n+rw > w-1 {
			break
		}
		b.WriteRune(r)
		n += rw
	}
	b.WriteByte('~')
	return b.String()
}

// Pad cuts or space-pads s to exactly w columns.
func Pad(s string, w int) string {
	s = Fit(s, w)
	if n := Width(s); n < w {
		s += strings.Repeat(" ", w-n)
	}
	return s
}
