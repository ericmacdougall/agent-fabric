package ingest

import (
	"strings"
	"testing"
)

func TestAcceptedEightKiBLineBound(t *testing.T) {
	const acceptedMax = 8 << 10
	base := `{"schema":"mc.event/1","id":"harness:line","ts":"2026-11-10T00:00:00Z","kind":"wal"}`
	boundary := base + strings.Repeat(" ", acceptedMax-len(base))
	if _, err := ValidateLine([]byte(boundary), "harness"); err != nil {
		t.Fatalf("exactly8192 bytes should pass: %v", err)
	}
	if _, err := ValidateLine([]byte(boundary+" "), "harness"); err == nil || err.Error() != "line-too-long" {
		t.Errorf("8193 valid JSON bytes must be refused: %v", err)
	}
	for _, oversized := range []string{boundary + " ", strings.Repeat(" ", acceptedMax+1)} {
		batch, err := Parse(strings.NewReader(oversized+"\n"+base+"\n"), "harness")
		if err != nil || len(batch.Accepted) != 1 || len(batch.Rejected) != 1 || batch.Rejected[0].Line != 1 || batch.Rejected[0].Reason != "line-too-long" {
			t.Errorf("oversized line must be skipped and counted before following good line: accepted=%d rejected=%v err=%v", len(batch.Accepted), batch.Rejected, err)
		}
	}
	for _, tc := range []struct {
		name string
		line string
		want int
	}{
		{"crlf_at_bound", boundary[:len(boundary)-1] + "\r\n", 0},
		{"crlf_over_bound", boundary + "\r\n", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			batch, err := Parse(strings.NewReader(tc.line), "harness")
			if err != nil || len(batch.Rejected) != tc.want {
				t.Fatalf("CR counts, LF separator does not: rejects=%d err=%v", len(batch.Rejected), err)
			}
		})
	}
	if MaxCallBytes != 1<<20 || MaxLines != 2000 {
		t.Fatalf("retained tighter call caps changed: bytes=%d lines=%d", MaxCallBytes, MaxLines)
	}
}
