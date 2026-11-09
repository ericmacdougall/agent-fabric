package anthropic

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A secret manager often stores a key with a trailing newline or stray
// surrounding spaces. Surrounding whitespace is trimmed before the shape
// check; whitespace inside the value is never accepted.
func TestKeyCommandTrimsSurroundingWhitespaceOnly(t *testing.T) {
	key := "sk-ant-api03-" + strings.Repeat("aB3_-", 18)
	run := func(out string) (string, error) {
		src, err := KeyFromCommand([]string{"printf", "%s", out}, time.Minute, nil)
		if err != nil {
			t.Fatal(err)
		}
		return src(context.Background())
	}
	for _, in := range []string{key, key + "\n", "  " + key + "\n", "\t " + key + " \r\n"} {
		got, err := run(in)
		if err != nil || got != key {
			t.Fatalf("surrounding whitespace on a valid key was not accepted (err=%v, same=%v)", err, got == key)
		}
	}
	for _, in := range []string{key[:30] + " " + key[30:], key[:30] + "\n" + key[30:]} {
		if got, err := run(in); err == nil || got != "" {
			t.Fatalf("a key with interior whitespace was accepted")
		}
	}
}
