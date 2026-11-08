package anthropic

import (
	"context"
	"testing"
	"time"

	"fabric/internal/provider/anthropic/fakeapi"
)

// Only an API-key-shaped secret is ever sent: a key source that returns
// anything else (an OAuth token, a blank line, junk) sends no request.
func TestNonKeyShapedSecretIsNeverSent(t *testing.T) {
	for _, bad := range []string{"", "not-a-key", "sk-ant-api03-short", canary + " "} {
		s := fakeapi.New(ok("never"))
		var sleeps []time.Duration
		c, err := New(Config{BaseURL: ProductionBaseURL, HTTP: s.HTTPClient(), Version: "v",
			Key:   func(context.Context) (string, error) { return bad, nil },
			Retry: RetryPolicy{MaxAttempts: 1}, Sleep: func(_ context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil }})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := c.Send(context.Background(), body(t), Hooks{}); err == nil {
			t.Fatalf("key %q accepted", bad)
		}
		if n := len(s.Requests()); n != 0 {
			t.Fatalf("key %q: %d requests reached the API", bad, n)
		}
		s.Close()
	}
}
