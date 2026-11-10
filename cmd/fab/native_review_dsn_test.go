package main

import (
	"os"
	"testing"
)

func TestTakeReviewDSNScrubsProcessEnvironment(t *testing.T) {
	const dsn = "postgres://app:secret@127.0.0.1/review"
	for _, name := range []string{"FABRIC_REVIEW_DSN_FILE", "FABRIC_REVIEW_DSN_PASSWORD_FILE", "FABRIC_REVIEW_DSN_FILE_KEY"} {
		unsetConnectionEnv(t, name)
	}
	t.Setenv("FABRIC_REVIEW_DSN", dsn)
	got, credential, err := takeReviewDSN()
	if err != nil || got != dsn {
		t.Fatalf("takeReviewDSN = %q, %v", got, err)
	}
	defer credential.Close()
	if _, present := os.LookupEnv("FABRIC_REVIEW_DSN"); present {
		t.Fatal("app-role review DSN remains in the worker parent environment")
	}
}
