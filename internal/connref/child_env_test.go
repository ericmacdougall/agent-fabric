package connref

import (
	"slices"
	"testing"
)

func TestChildEnvironmentCannotRedirectCanonicalReference(t *testing.T) {
	base := []string{"PATH=/usr/bin:/bin", "FABRIC_RUNNER_DSN_ENV=CUSTOM_RUNNER", "FABRIC_DSN=discard-source", "FABRIC_DSN_DATABASE=unapproved_base", "FABRIC_TEST_DSN_FILE=discard-source-file", "FABRIC_REVIEW_DSN_PASSWORD_FILE=discard-secret-source", "CUSTOM_RUNNER_DATABASE=unapproved_base", "CUSTOM_RUNNER_FILE_KEY=discard-field", "FABRIC_STORETEST_CHILD_DSN=discard-stale-child"}
	got := ChildEnvironment(base)
	if !slices.Equal(got, []string{"PATH=/usr/bin:/bin"}) {
		t.Fatal("source metadata or target survived the child boundary")
	}
	// A resolved reference is appended only after inherited sources are gone.
	got = append(got, "FABRIC_STORETEST_CHILD_DSN=canonical-nonsecret-reference")
	if len(got) != 2 {
		t.Fatal("explicit canonical child reference was not retained")
	}
}
