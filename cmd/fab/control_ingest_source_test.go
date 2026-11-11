package main

import (
	"errors"
	"testing"
)

// goodqa#15661 F3: a well-formed source that the database allowlist would refuse
// is a refusal (exit 3) before any connection, not an unknown-effect error (exit 1).
func TestControlIngestRefusesUnlistedSourceBeforeConnecting(t *testing.T) {
	t.Setenv("FABRIC_DSN", "") // a connection attempt would fail differently, so exit 3 proves the order
	err := controlMain([]string{"ingest", "--source", "not-listed"})
	var ue usageError
	if !errors.As(err, &ue) || exitCode(err) != 3 {
		t.Fatalf("want usage refusal with exit 3, got %v (exit %d)", err, exitCode(err))
	}
}
