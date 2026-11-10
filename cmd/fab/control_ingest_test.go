package main

import (
	"errors"
	"fmt"
	"testing"
)

// goodqa#15586 B3: a call that completed with rejected lines exits 4, distinct
// from a clean run (0), a refusal that wrote nothing (3) and an unknown error (1),
// also when wrapped.
func TestControlIngestExitCodes(t *testing.T) {
	for _, c := range []struct {
		err  error
		want int
	}{
		{nil, 0},
		{partialRejects{2}, 4},
		{fmt.Errorf("wrapped: %w", partialRejects{1}), 4},
		{usageError{errors.New("limit")}, 3},
		{errors.New("boom"), 1},
	} {
		if got := exitCode(c.err); got != c.want {
			t.Errorf("exitCode(%v) = %d, want %d", c.err, got, c.want)
		}
	}
	if msg := (partialRejects{3}).Error(); msg == "" {
		t.Fatal("empty message")
	}
}
