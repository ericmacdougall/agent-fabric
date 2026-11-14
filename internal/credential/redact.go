package credential

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"unicode/utf8"
)

// Redaction block classes (AC-15). Recorded as delivery_blocked classes.
const (
	ClassNumericHit  = "redaction_numeric_hit"
	ClassBrokeStream = "redaction_broke_stream"
)

// MaxStreamRead is the most fabd reads of one stream (AC-16 (8)).
const MaxStreamRead = 64 << 20

// LabelFragment is the per-label suffix of fragment hits.
const LabelFragment = "fragment"

// LabelLiteral counts the public token shape: an `sk-ant-` literal whose
// tail adds fewer than GramLen characters past the public prefix, and
// fragment runs that lie wholly inside the bound token's public prefix. It
// is never a credential exposure (owner ruling 2026-11-14).
const LabelLiteral = "literal:sk-ant-"

// LabelLiteralToken counts an `sk-ant-` literal followed by GramLen or more
// token characters past the public prefix: a token-shaped value (for
// example an unbound token version). It is an exposure.
const LabelLiteralToken = "literal:sk-ant-token"

// RedactResult is one redaction of an attempt's two streams.
type RedactResult struct {
	Stdout, Stderr []byte
	// Counts holds hits per label (whole-value pattern labels, the token's
	// fragment label and the literal label), over both streams.
	Counts map[string]int
	// Rewrites counts bytes rewritten to 'X' (benign or not: a rewrite is
	// never a block, goodqa#16040).
	Rewrites int
	// Blocking counts hits that could not be rewritten in place (inside a
	// JSON number or touching a structural byte).
	Blocking int
	// Blocked is "" or the delivery_blocked class: redaction_numeric_hit or
	// redaction_broke_stream. When Blocked is set nothing may be published.
	// Every hit is still removed from the copy, so the copy is safe to keep.
	Blocked string
	// Truncated reports that a stream exceeded MaxStreamRead.
	Truncated bool
}

// Exposed reports whether any credential form was found (credential_exposure).
// The public literal (LabelLiteral) is counted but is not an exposure.
func (r RedactResult) Exposed() bool {
	for l, n := range r.Counts {
		if n > 0 && l != LabelLiteral {
			return true
		}
	}
	return false
}

// Redact redacts stdout (the stream-json stream) and stderr (opaque text).
// It matches every bound value in every policy form under both
// normalisations, every maximal run of 8 or more bytes that occurs inside a
// form of the bound token, and the `sk-ant-` shape. Each stream is matched
// on its own, so a value split across the two streams leaves no fragment of
// 8 or more bytes in either.
func (b *Bound) Redact(stdout, stderr []byte) RedactResult {
	res := RedactResult{Counts: map[string]int{}}
	covOut := b.mark(stdout, res.Counts)
	covErr := b.mark(stderr, res.Counts)
	res.Stdout, res.Blocking, res.Blocked = applyJSONLines(stdout, covOut, &res.Rewrites)
	res.Stderr = applyOpaque(stderr, covErr, &res.Rewrites)
	return res
}

// mark returns the covered bytes of src.
func (b *Bound) mark(src []byte, counts map[string]int) []bool {
	cov := make([]bool, len(src))
	if len(src) == 0 {
		return cov
	}
	for _, n := range AllNorms {
		norm, idx := Normalize(n, src)
		cover := func(a, e int) { // normalised [a, e) -> original idx[a]..idx[e-1]
			for k := idx[a]; k <= idx[e-1]; k++ {
				cov[k] = true
			}
		}
		for _, p := range b.patterns {
			if p.norm != n {
				continue
			}
			for off := 0; ; {
				i := bytes.Index(norm[off:], p.b)
				if i < 0 {
					break
				}
				a := off + i
				cover(a, a+len(p.b))
				counts[p.label]++
				off = a + 1
			}
		}
		// Fragment runs of the token (AC-15).
		// A run made only of public-prefix windows is the public literal.
		run, end, secret := -1, 0, false
		flush := func() {
			cover(run, end)
			if n == NormWS {
				if secret {
					counts[b.tokLabel+":"+LabelFragment]++
				} else {
					counts[LabelLiteral]++
				}
			}
		}
		for i := 0; i+GramLen <= len(norm); i++ {
			w := norm[i : i+GramLen]
			if b.hasGram(w) {
				if run < 0 || i > end {
					if run >= 0 {
						flush()
					}
					run, secret = i, false
				}
				end = i + GramLen
				if b.isSecretGram(w) {
					secret = true
				}
			}
		}
		if run >= 0 {
			flush()
		}
	}
	// The public literal shape on the original bytes: sk-ant- and the token
	// characters after it (a normalised scan would run into the next word).
	for off := 0; ; {
		i := bytes.Index(src[off:], []byte(PublicLiteral))
		if i < 0 {
			break
		}
		a := off + i
		e := a + len(PublicLiteral)
		for e < len(src) && tokenChar(src[e]) {
			e++
		}
		for k := a; k < e; k++ {
			cov[k] = true
		}
		tail := e - a - len(PublicLiteral)
		if pre := publicPrefixRE.Find(src[a:e]); pre != nil {
			tail = e - a - len(pre)
		}
		if tail >= GramLen {
			counts[LabelLiteralToken]++
		} else {
			counts[LabelLiteral]++
		}
		off = e
	}
	return cov
}

func tokenChar(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_'
}

// applyOpaque rewrites covered non-whitespace bytes to 'X'.
func applyOpaque(src []byte, cov []bool, rewrites *int) []byte {
	out := append([]byte(nil), src...)
	for i, c := range cov {
		if c && !isWS(src[i]) {
			out[i] = 'X'
			*rewrites++
		}
	}
	return out
}

// byte contexts in a JSON text
const (
	ctxWS byte = iota
	ctxStruct
	ctxNumber
	ctxString
)

// classify labels every byte of a valid JSON text and, for string bytes, the
// start of the unit (escape sequence or UTF-8 sequence) it belongs to.
func classify(line []byte) (ctx []byte, unit []int) {
	ctx = make([]byte, len(line))
	unit = make([]int, len(line))
	for i := 0; i < len(line); {
		c := line[i]
		switch {
		case isWS(c):
			ctx[i] = ctxWS
			i++
		case c == '"':
			ctx[i] = ctxStruct
			i++
			for i < len(line) && line[i] != '"' {
				n := 1
				if line[i] == '\\' {
					n = 2
					if i+1 < len(line) && line[i+1] == 'u' {
						n = 6
					}
				} else if line[i] >= 0x80 {
					_, n = utf8.DecodeRune(line[i:])
				}
				for k := i; k < i+n && k < len(line); k++ {
					ctx[k], unit[k] = ctxString, i
				}
				i += n
			}
			if i < len(line) {
				ctx[i] = ctxStruct
				i++
			}
		case c == '-' || (c >= '0' && c <= '9'):
			for i < len(line) && (line[i] == '-' || line[i] == '+' || line[i] == '.' || line[i] == 'e' || line[i] == 'E' || (line[i] >= '0' && line[i] <= '9')) {
				ctx[i] = ctxNumber
				i++
			}
		default:
			ctx[i] = ctxStruct
			i++
		}
	}
	return ctx, unit
}

// applyJSONLines redacts a stream-json stream line by line. In a line that
// is valid JSON a covered string unit is rewritten to 'X' byte for byte
// (length preserving); a covered number or structural byte is a blocking
// hit. A line that is not valid JSON (worker junk) is redacted as opaque
// text. A blocking hit is still removed from the copy (opaque rewrite of
// the line), so no hit survives in any case; the stream is then blocked.
func applyJSONLines(src []byte, cov []bool, rewrites *int) (out []byte, blocking int, blocked string) {
	out = append([]byte(nil), src...)
	for start := 0; start < len(src); {
		end := bytes.IndexByte(src[start:], '\n')
		if end < 0 {
			end = len(src)
		} else {
			end += start
		}
		line := src[start:end]
		lc := cov[start:end]
		any := false
		for _, c := range lc {
			if c {
				any = true
				break
			}
		}
		if any {
			if !json.Valid(line) {
				copy(out[start:end], applyOpaque(line, lc, rewrites))
			} else {
				ctx, unit := classify(line)
				hard := false
				units := map[int]bool{}
				for i, c := range lc {
					if !c {
						continue
					}
					switch ctx[i] {
					case ctxString:
						units[unit[i]] = true
					case ctxNumber, ctxStruct:
						hard = true
						blocking++
					}
				}
				if hard {
					copy(out[start:end], applyOpaque(line, lc, rewrites))
					if blocked == "" {
						blocked = ClassNumericHit
					}
				} else {
					nl := out[start:end]
					for i := range line {
						if ctx[i] == ctxString && units[unit[i]] {
							nl[i] = 'X'
							*rewrites++
						}
					}
					if !sameStructure(line, nl) && blocked == "" {
						blocked = ClassBrokeStream
					}
				}
			}
		}
		start = end + 1
	}
	return out, blocking, blocked
}

// sameStructure reports that b parses and has a's token structure: the same
// token kinds and delimiters, the same numbers, and strings of any content.
func sameStructure(a, b []byte) bool {
	if !json.Valid(b) {
		return false
	}
	ta, ea := tokens(a)
	tb, eb := tokens(b)
	if ea != nil || eb != nil || len(ta) != len(tb) {
		return false
	}
	for i := range ta {
		if ta[i] != tb[i] {
			return false
		}
	}
	return true
}

func tokens(s []byte) ([]string, error) {
	d := json.NewDecoder(bytes.NewReader(s))
	d.UseNumber()
	var out []string
	for {
		t, err := d.Token()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		switch v := t.(type) {
		case string:
			out = append(out, "s")
		case json.Number:
			out = append(out, "n"+string(v))
		case json.Delim:
			out = append(out, "d"+v.String())
		default:
			out = append(out, fmt.Sprintf("%T:%v", v, v))
		}
	}
}

// RedactStreams reads the attempt's raw streams (at most MaxStreamRead each)
// and writes the redacted copies, mode 0600, as outDir/stream.jsonl and
// outDir/stderr.log (O_EXCL, fsync). Settle, usage capture, events and the
// comment read only these copies. When the result is Blocked it writes
// nothing.
func (b *Bound) RedactStreams(rawStdout, rawStderr, outDir string) (RedactResult, error) {
	so, t1, err := readCapped(rawStdout)
	if err != nil {
		return RedactResult{}, fmt.Errorf("redact: read stdout: %w", err)
	}
	se, t2, err := readCapped(rawStderr)
	if err != nil {
		return RedactResult{}, fmt.Errorf("redact: read stderr: %w", err)
	}
	res := b.Redact(so, se)
	res.Truncated = t1 || t2
	if res.Blocked != "" {
		// AC-15: a blocked delivery publishes nothing, so no copy exists for
		// settle, usage capture, events or the comment to read; the raw
		// streams stay, unpublished, for the operator.
		return res, nil
	}
	for name, data := range map[string][]byte{"stream.jsonl": res.Stdout, "stderr.log": res.Stderr} {
		if err := writeExcl(filepath.Join(outDir, name), data); err != nil {
			return res, fmt.Errorf("redact: write %s: %w", name, err)
		}
	}
	return res, nil
}

func readCapped(path string) ([]byte, bool, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|oNoFollow, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxStreamRead+1))
	if err != nil {
		return nil, false, err
	}
	if len(data) > MaxStreamRead {
		return data[:MaxStreamRead], true, nil
	}
	return data, false, nil
}

func writeExcl(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|oNoFollow, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// SortedLabels returns the labels of counts, sorted (for records).
func SortedLabels(counts map[string]int) []string {
	out := make([]string, 0, len(counts))
	for k := range counts {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
