package tools

import (
	"bytes"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// unifiedDiff renders a small unified diff of a to b, capped at max bytes.
// It trims the common prefix and suffix and runs an LCS on what is left; a
// middle too large for the LCS table is shown as one replaced block.
func unifiedDiff(name string, a, b []byte, max int) string {
	al, bl := splitLines(a), splitLines(b)
	p := 0
	for p < len(al) && p < len(bl) && al[p] == bl[p] {
		p++
	}
	s := 0
	for s < len(al)-p && s < len(bl)-p && al[len(al)-1-s] == bl[len(bl)-1-s] {
		s++
	}
	am, bm := al[p:len(al)-s], bl[p:len(bl)-s]
	var ops []string // " x", "-x", "+x"
	if len(am)*len(bm) <= 4_000_000 {
		ops = lcsOps(am, bm)
	} else {
		for _, l := range am {
			ops = append(ops, "-"+l)
		}
		for _, l := range bm {
			ops = append(ops, "+"+l)
		}
	}
	const ctx = 3
	var out strings.Builder
	fmt.Fprintf(&out, "--- a/%s\n+++ b/%s\n", name, name)
	lo := max2(0, p-ctx)
	var pre []string
	for _, l := range al[lo:p] {
		pre = append(pre, " "+l)
	}
	hi := min2(len(al), len(al)-s+ctx)
	var post []string
	for _, l := range al[len(al)-s : hi] {
		post = append(post, " "+l)
	}
	all := append(append(pre, ops...), post...)
	oldN, newN := 0, 0
	for _, o := range all {
		if o[0] != '+' {
			oldN++
		}
		if o[0] != '-' {
			newN++
		}
	}
	fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", lo+1, oldN, lo+1, newN)
	for _, o := range all {
		out.WriteString(o)
		out.WriteByte('\n')
		if out.Len() > max {
			s := out.String()[:max]
			for !utf8.ValidString(s) {
				s = s[:len(s)-1]
			}
			return s + "\n[diff truncated]"
		}
	}
	return out.String()
}

func max2(a, b int) int {
	if a > b {
		return a
	}
	return b
}
func min2(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func splitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

func lcsOps(a, b []string) []string {
	n, m := len(a), len(b)
	t := make([][]int32, n+1)
	for i := range t {
		t[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				t[i][j] = t[i+1][j+1] + 1
			} else if t[i+1][j] >= t[i][j+1] {
				t[i][j] = t[i+1][j]
			} else {
				t[i][j] = t[i][j+1]
			}
		}
	}
	var ops []string
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, " "+a[i])
			i++
			j++
		case t[i+1][j] >= t[i][j+1]:
			ops = append(ops, "-"+a[i])
			i++
		default:
			ops = append(ops, "+"+b[j])
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, "-"+a[i])
	}
	for ; j < m; j++ {
		ops = append(ops, "+"+b[j])
	}
	return ops
}

// ---- normalised matching (Fm-3) ----

type normText struct {
	b          []byte
	start, end []int // original byte span of the segment that produced each byte
}

func foldRune(r rune) rune {
	switch r {
	case '‘', '’', '‚', '‛', '′':
		return '\''
	case '“', '”', '„', '‟', '″':
		return '"'
	case '‐', '‑', '‒', '–', '—', '―', '−':
		return '-'
	case ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', '　':
		return ' '
	}
	return r
}

// normalize applies NFKC per normalisation segment, folds quotes, dashes and
// spaces, and drops trailing spaces and tabs before a line end, recording for
// every output byte the original span it came from.
func normalize(s []byte) normText {
	var nt normText
	str := string(s)
	pos := 0
	var segB []byte
	var segS, segE []int
	for pos < len(str) {
		n := norm.NFKC.NextBoundaryInString(str[pos:], true)
		if n <= 0 {
			n = len(str) - pos
		}
		seg := norm.NFKC.String(str[pos : pos+n])
		for _, r := range seg {
			var rb [4]byte
			w := utf8.EncodeRune(rb[:], foldRune(r))
			for k := 0; k < w; k++ {
				segB = append(segB, rb[k])
				segS = append(segS, pos)
				segE = append(segE, pos+n)
			}
		}
		pos += n
	}
	// drop trailing blanks before a line end
	for i := 0; i < len(segB); i++ {
		if segB[i] == ' ' || segB[i] == '\t' {
			j := i
			for j < len(segB) && (segB[j] == ' ' || segB[j] == '\t') {
				j++
			}
			if j < len(segB) && (segB[j] == '\n' || segB[j] == '\r') {
				i = j - 1
				continue
			}
			nt.b = append(nt.b, segB[i:j]...)
			nt.start = append(nt.start, segS[i:j]...)
			nt.end = append(nt.end, segE[i:j]...)
			i = j - 1
			continue
		}
		nt.b = append(nt.b, segB[i])
		nt.start = append(nt.start, segS[i])
		nt.end = append(nt.end, segE[i])
	}
	return nt
}

// findAll returns every (overlapping) start of sub in s.
func findAll(s, sub []byte) []int {
	var out []int
	for off := 0; off+len(sub) <= len(s); {
		i := bytes.Index(s[off:], sub)
		if i < 0 {
			break
		}
		out = append(out, off+i)
		off += i + 1
	}
	return out
}

func lineOf(b []byte, off int) int { return 1 + bytes.Count(b[:off], []byte{'\n'}) }
