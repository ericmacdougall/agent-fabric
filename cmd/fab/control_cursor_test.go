package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

func cursorFixture(version, body string) string {
	sum := sha256.Sum256([]byte(version + "|" + body))
	return version + "." + base64.RawURLEncoding.EncodeToString([]byte(body)) + "." + hex.EncodeToString(sum[:4])
}

func TestControlCursorRollback(t *testing.T) {
	t.Setenv("FABRIC_DSN", "") // conversion is local and must never need a database
	body := "0123456789abcdef:42:abcdef123456"
	live := cursorFixture("fx2", body+":l")
	out, err := capture(t, func() error { return controlMain([]string{"cursor-rollback", live}) })
	if err != nil || out != cursorFixture("fx1", body)+"\n" {
		t.Fatalf("live conversion: output=%q err=%v", out, err)
	}
	for name, args := range map[string][]string{
		"backfill": {"cursor-rollback", cursorFixture("fx2", body+":b")},
		"legacy":   {"cursor-rollback", cursorFixture("fx1", body)},
		"old_live": {"cursor-rollback", cursorFixture("fx1", body+":l")},
		"tampered": {"cursor-rollback", live + "0"},
		"unknown":  {"cursor-rollback", cursorFixture("fx9", body+":l")},
		"missing":  {"cursor-rollback"},
		"extra":    {"cursor-rollback", live, "extra"},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := capture(t, func() error { return controlMain(args) })
			if err == nil || out != "" || exitCode(err) != 3 {
				t.Fatalf("must refuse without output: bytes=%d exit=%d err=%v", len(out), exitCode(err), err)
			}
			if name == "backfill" && !strings.Contains(err.Error(), "catch up") {
				t.Fatalf("missing actionable refusal: %v", err)
			}
		})
	}
}
