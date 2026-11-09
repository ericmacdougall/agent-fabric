package main

import (
	"os"
	"strings"
	"testing"
)

// The actual bug site (goodqa#15579): daemonMain must run under
// withStoreService, never withStore. A source guard, like
// TestDaemonLaunchComesFromDaemonProvider.
func TestDaemonMainUsesTheServiceContext(t *testing.T) {
	b, err := os.ReadFile("job.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	i := strings.Index(src, "func daemonMain(")
	if i < 0 {
		t.Fatal("daemonMain not found")
	}
	body := src[i:]
	if j := strings.Index(body[1:], "\nfunc "); j >= 0 {
		body = body[:j+1]
	}
	if !strings.Contains(body, "withStoreService(") || strings.Contains(body, "withStore(") {
		t.Fatal("daemonMain must run under withStoreService and not withStore (withStore ends the daemon after 60 seconds, goodqa#15579)")
	}
}
