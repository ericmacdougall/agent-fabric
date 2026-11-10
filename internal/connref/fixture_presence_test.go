package connref

import (
	"os"
	"testing"
)

func TestFixtureReferencePresenceIncludesEmptyModes(t *testing.T) {
	for _, suffix := range []string{"_FILE", "_PASSWORD_FILE", "_FILE_KEY", "_DATABASE"} {
		t.Run(suffix, func(t *testing.T) {
			clearFixtureReferenceEnv(t)
			t.Setenv("FABRIC_TEST_DSN"+suffix, "")
			if !HasReferenceConfig("FABRIC_TEST_DSN") {
				t.Fatal("an explicitly empty reference was treated as absent")
			}
		})
	}
}

func clearFixtureReferenceEnv(t *testing.T) {
	t.Helper()
	for _, suffix := range []string{"", "_FILE", "_PASSWORD_FILE", "_FILE_KEY"} {
		key := "FABRIC_TEST_DSN" + suffix
		value, present := os.LookupEnv(key)
		if err := os.Unsetenv(key); err != nil {
			t.Fatal("clear synthetic fixture configuration failed")
		}
		t.Cleanup(func() {
			if present {
				os.Setenv(key, value)
			} else {
				os.Unsetenv(key)
			}
		})
	}
}
