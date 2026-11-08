package anthropic

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"fabric/internal/provider/anthropic/fakeapi"
)

// goodqa#15168 (Kimi K12, provider half): an error whose text carries the
// key is scrubbed before it reaches the caller or the attempt record.
// TestKeyNotInErrors uses errors that never contain the key, so it cannot
// catch a regression in scrubErr itself; these do.
func TestScrubErr(t *testing.T) {
	in := errors.New("Post \"https://api.anthropic.com/v1/messages\": proxy said: bad header x-api-key=" + canary + " twice " + canary)
	out := scrubErr(in, canary)
	if strings.Contains(out.Error(), canary) || strings.Count(out.Error(), "[redacted]") != 2 {
		t.Fatalf("scrubErr left the key or lost an occurrence: %q", out)
	}
	plain := errors.New("connection reset")
	if scrubErr(plain, canary) != plain || scrubErr(in, "") != in {
		t.Fatal("scrubErr rewrote an error it had no reason to touch")
	}
}

type keyEchoTransport struct{}

func (keyEchoTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return nil, errors.New("transport refused request with key " + r.Header.Get("x-api-key"))
}

func TestTransportErrorWithKeyIsScrubbed(t *testing.T) {
	s := fakeapi.New(fakeapi.Step{DieBeforeBody: true})
	defer s.Close()
	var sl []time.Duration
	c := newClient(t, s, &sl)
	c.cfg.HTTP = &http.Client{Transport: keyEchoTransport{}}
	_, recs, err := c.Send(context.Background(), body(t), Hooks{})
	if err == nil {
		t.Fatal("expected a transport error")
	}
	all := err.Error()
	for _, r := range recs {
		all += " " + r.Err
	}
	if strings.Contains(all, canary) {
		t.Fatalf("the key reached the error or the attempt record: %q", all)
	}
	if !strings.Contains(all, "[redacted]") {
		t.Fatalf("the transport error was not the one the test injected: %q", all)
	}
}
