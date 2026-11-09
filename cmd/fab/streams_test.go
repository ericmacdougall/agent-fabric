package main

import (
	"errors"
	"testing"
)

// Click-test findings F-2 and F-3 (goodqa#15372): --help is not a failure and
// a stray argument is a usage error, both before any database is touched.
func TestStreamsHeldArgs(t *testing.T) {
	if err := streamsMain([]string{"held", "--help"}); err != nil {
		t.Fatalf("--help returned %v, want nil", err)
	}
	err := streamsMain([]string{"held", "extra-arg"})
	var ue usageError
	if !errors.As(err, &ue) {
		t.Fatalf("a stray argument gave %v, want a usage error", err)
	}
	if err := streamsMain([]string{"held", "--limit", "0"}); !errors.As(err, &ue) {
		t.Fatalf("--limit 0 gave %v, want a usage error", err)
	}
	if err := streamsMain([]string{"nope"}); !errors.As(err, &ue) {
		t.Fatalf("an unknown subcommand gave %v, want a usage error", err)
	}
}
