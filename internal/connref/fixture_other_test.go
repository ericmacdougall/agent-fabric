//go:build !linux

package connref

import (
	"errors"
	"testing"
)

func TestNonLinuxFixtureEmptyReferenceNeverUsesRawCompatibility(t *testing.T) {
	for _, suffix := range []string{"_FILE", "_PASSWORD_FILE", "_FILE_KEY"} {
		t.Run(suffix, func(t *testing.T) {
			clearFixtureReferenceEnv(t)
			t.Setenv("FABRIC_TEST_DSN"+suffix, "")
			legacy := &Ref{dsn: "postgres://fixture@127.0.0.1/postgres", purpose: TestAdmin}
			defer legacy.Close()
			if ref, err := ForTestDatabase(legacy, "fabric_fixture"); !errors.Is(err, ErrUnsupported) {
				if ref != nil {
					ref.Close()
				}
				t.Fatal("an empty reference allowed raw database derivation")
			}
			if fixture, err := NewFixture("postgres://fixture@127.0.0.1/postgres", Runner, "fixture_runner", "synthetic", ""); !errors.Is(err, ErrUnsupported) {
				if fixture != nil {
					fixture.Close()
				}
				t.Fatal("an empty reference allowed raw fixture authentication")
			}
		})
	}
}
