//go:build !linux

package connref

import (
	"errors"
	"os"
	"testing"
)

func without(t *testing.T, name string) {
	t.Helper()
	value, present := os.LookupEnv(name)
	os.Unsetenv(name)
	t.Cleanup(func() {
		if present {
			os.Setenv(name, value)
		} else {
			os.Unsetenv(name)
		}
	})
}

func TestUnsupportedReferenceNeverFallsBack(t *testing.T) {
	for _, name := range []string{"FABRIC_DSN", "FABRIC_DSN_FILE", "FABRIC_DSN_PASSWORD_FILE", "FABRIC_DSN_FILE_KEY", "FABRIC_DSN_DATABASE"} {
		without(t, name)
	}
	t.Setenv("FABRIC_DSN_FILE", "/synthetic/not-read-on-this-platform")
	r, err := ResolveEnv("FABRIC_DSN", App)
	if r != nil {
		r.Close()
	}
	if !errors.Is(err, ErrUnsupported) {
		t.Fatal("unsupported file mode did not explicitly refuse")
	}
	for _, suffix := range []string{"_FILE", "_PASSWORD_FILE", "_FILE_KEY", "_DATABASE"} {
		t.Run(suffix, func(t *testing.T) {
			without(t, "FABRIC_DSN_FILE")
			t.Setenv("FABRIC_DSN", "postgres://fixture@127.0.0.1/fabric_fixture")
			t.Setenv("FABRIC_DSN"+suffix, "")
			if !HasReferenceConfig("FABRIC_DSN") {
				t.Fatal("configured empty reference was absent")
			}
			r, err := ResolveEnv("FABRIC_DSN", App)
			if r != nil {
				r.Close()
			}
			if err == nil {
				t.Fatal("unsupported/empty mode fell back to a raw value")
			}
		})
	}
}
