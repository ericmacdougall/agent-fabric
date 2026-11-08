package anthropic

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"fabric/internal/provider/anthropic/fakeapi"
)

func TestProductionBaseURLIsExact(t *testing.T) {
	for _, raw := range []string{
		"http://api.anthropic.com",
		"https://api.anthropic.com/",
		"https://api.anthropic.com/v1",
		"https://api.anthropic.com?x=1",
		"https://user@api.anthropic.com",
		"https://api.anthropic.com.evil.invalid",
	} {
		t.Run(raw, func(t *testing.T) {
			var keys atomic.Int32
			_, err := New(Config{BaseURL: raw, Version: "v", Key: func(context.Context) (string, error) {
				keys.Add(1)
				return canary, nil
			}})
			if err == nil {
				t.Fatalf("accepted non-production BaseURL %q", raw)
			}
			if keys.Load() != 0 {
				t.Fatal("key source called while rejecting BaseURL")
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRedirectNeverReceivesKey(t *testing.T) {
	var calls atomic.Int32
	var redirectedKeys atomic.Int32
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		n := calls.Add(1)
		if n > 1 {
			if req.Header.Get("x-api-key") != "" {
				redirectedKeys.Add(1)
			}
			return &http.Response{StatusCode: 400, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"type":"bad","message":"redirected"}}`)), Request: req}, nil
		}
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"https://attacker.invalid/collect"}}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
	})
	c, err := New(Config{BaseURL: ProductionBaseURL, Version: "v", HTTP: &http.Client{Transport: transport},
		Key: func(context.Context) (string, error) { return canary, nil }, Retry: RetryPolicy{MaxAttempts: 1}})
	if err != nil {
		t.Fatal(err)
	}
	_, recs, err := c.Send(context.Background(), body(t), Hooks{})
	if err == nil {
		t.Fatal("redirect response unexpectedly succeeded")
	}
	if calls.Load() != 1 || redirectedKeys.Load() != 0 {
		t.Fatalf("redirect followed: calls=%d redirected keys=%d", calls.Load(), redirectedKeys.Load())
	}
	if len(recs) != 1 || recs[0].Status != http.StatusFound {
		t.Fatalf("attempt records %+v", recs)
	}
}

func TestAPIErrorRedactsReflectedActiveKey(t *testing.T) {
	s := fakeapi.New(fakeapi.Step{Status: 400, ErrType: "invalid_request_error", ErrMessage: "bad credential " + canary})
	defer s.Close()
	var sleeps []time.Duration
	_, recs, err := newClient(t, s, &sleeps).Send(context.Background(), body(t), Hooks{})
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("typed APIError lost: %T %v", err, err)
	}
	all := fmt.Sprintf("%v %v %+v", err, ae, recs)
	if strings.Contains(all, canary) || !strings.Contains(all, "[redacted]") {
		t.Fatalf("reflected key not redacted: %s", all)
	}
}

func TestCaptureRedactsKeyFromBodyAndHeaders(t *testing.T) {
	raw := `{"type":"error","error":{"type":"invalid_request_error","message":"` + canary + `"}}`
	s := fakeapi.New(fakeapi.Step{Status: 401, Raw: raw, Headers: map[string]string{
		"Request-Id":   "req_" + canary,
		"Content-Type": "text/event-stream; echo=" + canary,
	}})
	defer s.Close()
	var got Capture
	c, err := New(Config{BaseURL: ProductionBaseURL, Version: "v", HTTP: s.HTTPClient(),
		Key:     func(context.Context) (string, error) { return canary, nil },
		Capture: func(cp Capture) { got = cp },
		Retry:   RetryPolicy{MaxAttempts: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Send(context.Background(), body(t), Hooks{}); err == nil {
		t.Fatal("bad-auth fixture unexpectedly succeeded")
	}
	all := fmt.Sprint(got.Header) + string(got.ResponseBody) + got.RequestID
	if strings.Contains(all, canary) {
		t.Fatalf("capture leaked active key: %s", all)
	}
	if !strings.Contains(string(got.ResponseBody), "[redacted]") || !strings.Contains(got.Header.Get("Request-Id"), "[redacted]") {
		t.Fatalf("capture was not redacted: %+v", got)
	}
}

func TestSuccessfulResponseContainingKeyFailsClosed(t *testing.T) {
	// Text splits the key across deltas, so checking only raw SSE bytes is not
	// sufficient. The assembled content must be checked without modifying it.
	s := fakeapi.New(ok("prefix " + canary + " suffix"))
	defer s.Close()
	var sleeps []time.Duration
	m, _, err := newClient(t, s, &sleeps).Send(context.Background(), body(t), Hooks{})
	var pe *ProtocolError
	if !errors.As(err, &pe) || m != nil {
		t.Fatalf("reflected key returned assistant content: message=%+v error=%v", m, err)
	}
	if strings.Contains(err.Error(), canary) {
		t.Fatalf("protocol error leaked active key: %v", err)
	}
	if len(s.Requests()) != 1 {
		t.Fatalf("credential protocol error was retried %d times", len(s.Requests()))
	}
}
