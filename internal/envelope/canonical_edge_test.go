package envelope

import (
	"errors"
	"strings"
	"testing"
)

// Bytes that must never change: Go's encoding/json would escape U+2028 and
// U+2029, RFC 8785 emits them literally; DEL is literal too.
func TestLiteralSeparatorsAndDEL(t *testing.T) {
	in := "\"\\u2028\\u2029\\u007f\\u00e9\\ud83d\\ude00\""
	want := "\"  \u007fé\U0001F600\""
	got, err := Canonicalize([]byte(in))
	if err != nil || string(got) != want {
		t.Fatalf("got %q err %v, want %q", got, err, want)
	}
	// and as an object key
	got, err = Canonicalize([]byte(`{" ":1}`))
	if err != nil || string(got) != "{\" \":1}" {
		t.Fatalf("key: %q %v", got, err)
	}
}

func TestSurrogatePairsMustBeHighThenLow(t *testing.T) {
	for _, in := range []string{`"\ud800"`, `"\ud800\ud800"`, `"\udc00\ud800"`, `"\ud800"`, `"\ud800x"`} {
		if _, err := Canonicalize([]byte(in)); !errors.Is(err, ErrUTF8) {
			t.Errorf("%s: err = %v, want ErrUTF8", in, err)
		}
	}
}

// Exactly MaxDepth nested containers are accepted, one more is refused, for
// arrays and objects, with and without a scalar inside.
func TestDepthBoundary(t *testing.T) {
	arr := func(n int, inner string) string { return strings.Repeat("[", n) + inner + strings.Repeat("]", n) }
	obj := func(n int, inner string) string {
		return strings.Repeat(`{"a":`, n) + inner + strings.Repeat("}", n)
	}
	for _, inner := range []string{"", "1"} {
		if _, err := Canonicalize([]byte(arr(MaxDepth, inner))); err != nil {
			t.Errorf("array depth %d inner %q: %v", MaxDepth, inner, err)
		}
		if _, err := Canonicalize([]byte(arr(MaxDepth+1, inner))); !errors.Is(err, ErrTooDeep) {
			t.Errorf("array depth %d inner %q: err = %v, want ErrTooDeep", MaxDepth+1, inner, err)
		}
	}
	if _, err := Canonicalize([]byte(obj(MaxDepth, "1"))); err != nil {
		t.Errorf("object depth %d: %v", MaxDepth, err)
	}
	if _, err := Canonicalize([]byte(obj(MaxDepth+1, "1"))); !errors.Is(err, ErrTooDeep) {
		t.Errorf("object depth %d: err = %v, want ErrTooDeep", MaxDepth+1, err)
	}
}

// The input cap is exact, and the output cap keeps Canonicalize idempotent.
func TestSizeBoundaries(t *testing.T) {
	atCap := strings.Repeat(" ", MaxBytes-2) + "[]"
	if _, err := Canonicalize([]byte(atCap)); err != nil {
		t.Errorf("input of exactly MaxBytes: %v", err)
	}
	if _, err := Canonicalize([]byte(atCap + " ")); !errors.Is(err, ErrTooLarge) {
		t.Errorf("input of MaxBytes+1: %v", err)
	}
	// 1e20 prints as 21 digits, so a document under the input cap can exceed
	// it on output; that must be refused rather than produce bytes that
	// Canonicalize itself would refuse.
	big := "[" + strings.Repeat("1e20,", 838000) + "1]"
	if len(big) > MaxBytes {
		t.Fatal("test input is over the input cap")
	}
	if _, err := Canonicalize([]byte(big)); !errors.Is(err, ErrTooLarge) {
		t.Errorf("output over the cap: err = %v, want ErrTooLarge", err)
	}
}
