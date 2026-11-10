package ingest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

type vector struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	Line   string `json:"line"`
	Want   string `json:"want"`
}

func loadVectors(t *testing.T) []vector {
	t.Helper()
	f, err := os.Open("testdata/vectors.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []vector
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var v vector
		if err := json.Unmarshal(sc.Bytes(), &v); err != nil {
			t.Fatalf("vector line: %v", err)
		}
		out = append(out, v)
	}
	if len(out) < 50 {
		t.Fatalf("only %d vectors", len(out))
	}
	return out
}

// AC-1: every shared vector, valid and invalid, gives exactly the wanted outcome.
func TestValidate(t *testing.T) {
	ok, rejected := 0, 0
	for _, v := range loadVectors(t) {
		ev, err := ValidateLine([]byte(v.Line), v.Source)
		switch {
		case v.Want == "ok" && err != nil:
			t.Errorf("%s: want ok, rejected: %v", v.Name, err)
		case v.Want == "ok":
			ok++
			if ev.Source != v.Source || ev.TS.Nanosecond()%1_000_000 != 0 || ev.TS.Location().String() != "UTC" {
				t.Errorf("%s: not normalised: %+v", v.Name, ev)
			}
		case err == nil:
			t.Errorf("%s: want reject %q, accepted", v.Name, v.Want)
		case err.Error() != v.Want:
			t.Errorf("%s: want reason %q, got %q", v.Name, v.Want, err.Error())
		default:
			rejected++
		}
	}
	if ok < 8 || rejected < 40 {
		t.Fatalf("vector mix too thin: %d ok, %d rejected", ok, rejected)
	}
}

// A reason never echoes input text, so a hostile line cannot inject into stderr.
func TestReasonsNeverEchoInput(t *testing.T) {
	line := `{"schema":"mc.event/1","id":"wal-x:1","ts":"2026-11-09T12:30:45Z","kind":"wal","\u001b[31mevil":1}`
	_, err := ValidateLine([]byte(line), "wal-x")
	if err == nil || strings.ContainsAny(err.Error(), "\x1b[") || err.Error() != "unknown-field:?" {
		t.Fatalf("got %v", err)
	}
}

func lines(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, `{"schema":"mc.event/1","id":"wal-x:%d","ts":"2026-11-09T12:30:45.000Z","kind":"wal"}`+"\n", i)
	}
	return b.String()
}

// AC-3 and NEG-1 for the parser: whole-call limits refuse the call; a bad line
// is rejected alone with its number; nothing but accepted events is returned.
func TestIngestLimits(t *testing.T) {
	// exactly MaxLines good lines pass
	b, err := Parse(strings.NewReader(lines(MaxLines)), "wal-x")
	if err != nil || len(b.Accepted) != MaxLines || len(b.Rejected) != 0 {
		t.Fatalf("2000 lines: %v accepted=%d rejected=%d", err, len(b.Accepted), len(b.Rejected))
	}
	// one more line refuses the whole call
	if _, err := Parse(strings.NewReader(lines(MaxLines+1)), "wal-x"); !errors.Is(err, ErrCallTooLarge) {
		t.Fatalf("2001 lines: %v", err)
	}
	// more than 1 MiB refuses the whole call
	big := strings.Repeat(" ", MaxCallBytes+1)
	if _, err := Parse(strings.NewReader(big), "wal-x"); !errors.Is(err, ErrCallTooLarge) {
		t.Fatalf("1 MiB+1: %v", err)
	}
	// exactly 1 MiB of blank space is fine (and accepts nothing)
	if b, err := Parse(strings.NewReader(strings.Repeat("\n", 100)), "wal-x"); err != nil || b.Lines != 0 {
		t.Fatalf("blank lines: %v %+v", err, b)
	}
	// An 8 KiB line passes the size gate up to the limit; one byte over is rejected alone.
	pad := func(n int) string {
		head := `{"schema":"mc.event/1","id":"wal-x:p","ts":"2026-11-09T12:30:45Z","kind":"wal","host":"`
		tail := `"}`
		return head + strings.Repeat("a", n-len(head)-len(tail)) + tail
	}
	in := lines(1) + pad(MaxLineBytes+1) + "\n" + "not json\n" + lines(1)[:40] + "\n" + `{"schema":"mc.event/9","id":"wal-x:q","ts":"2026-11-09T12:30:45Z","kind":"wal"}` + "\n" + "\xff\xfe{}\n" +
		`{"schema":"mc.event/1","id":"wal-x:last","ts":"2026-11-09T12:30:45Z","kind":"wal"}` // no trailing newline
	b, err = Parse(strings.NewReader(in), "wal-x")
	if err != nil {
		t.Fatal(err)
	}
	if b.Lines != 7 || len(b.Accepted) != 2 || len(b.Rejected) != 5 {
		t.Fatalf("lines=%d accepted=%d rejected=%+v", b.Lines, len(b.Accepted), b.Rejected)
	}
	// line 2 oversized, 3 not JSON, 4 truncated, 5 wrong version, 6 non-UTF-8
	reasons := map[int]string{}
	for _, r := range b.Rejected {
		reasons[r.Line] = r.Reason
	}
	if reasons[2] != "line-too-long" || reasons[3] != "not-json-object" || reasons[4] != "not-json-object" || reasons[5] != "unknown-version" || reasons[6] != "not-utf8" {
		t.Fatalf("reasons by line: %v", reasons)
	}
	// the exact-limit line is accepted
	if _, err := ValidateLine([]byte(pad(MaxLineBytes)), "wal-x"); err != nil && err.Error() == "line-too-long" {
		t.Fatalf("a line of exactly %d bytes was refused as too long", MaxLineBytes)
	}
	// duplicates in one call collapse
	d := lines(1) + lines(1)
	if b, _ := Parse(strings.NewReader(d), "wal-x"); len(b.Accepted) != 1 || b.Dups != 1 {
		t.Fatalf("dups: %+v", b)
	}
	// reserved and invalid sources are refused before reading
	for _, s := range []string{"fabric", "Fabric", "", "a b", "x:y"} {
		if _, err := Parse(bytes.NewReader(nil), s); err == nil {
			t.Errorf("source %q accepted", s)
		}
	}
}
