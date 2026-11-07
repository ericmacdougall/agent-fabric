package anthropic

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"fabric/internal/provider/anthropic/fakeapi"
)

// goodqa#15087 AC-5 (red-team F14): the key travels only as x-api-key, to
// the configured host, never through a redirect or the environment proxy.

func TestOnlyXAPIKey(t *testing.T) {
	s := fakeapi.New(ok("hi"))
	defer s.Close()
	var sl []time.Duration
	if _, _, err := newClient(t, s, &sl).Send(context.Background(), body(t), Hooks{}); err != nil {
		t.Fatal(err)
	}
	reqs := s.Requests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests, want 1", len(reqs))
	}
	r := reqs[0]
	if r.Header.Get("x-api-key") != canary {
		t.Fatal("the key is not in x-api-key")
	}
	for name, vals := range r.Header {
		if strings.EqualFold(name, "x-api-key") {
			continue
		}
		if strings.EqualFold(name, "authorization") || strings.EqualFold(name, "proxy-authorization") {
			t.Fatalf("header %s is sent", name)
		}
		for _, v := range vals {
			if strings.Contains(v, canary) {
				t.Fatalf("the key appears in header %s", name)
			}
		}
	}
	if strings.Contains(string(r.Body), canary) {
		t.Fatal("the key appears in the request body")
	}
}

// A redirect is not followed: the 3xx comes back as an error response, and
// the redirect target receives nothing (so it never sees x-api-key).
func TestNoRedirect(t *testing.T) {
	var hit atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit.Add(1)
		w.WriteHeader(200)
	}))
	defer target.Close()
	for _, code := range []int{301, 302, 307, 308} {
		redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+"/v1/messages", code)
		}))
		var sl []time.Duration
		c, err := New(Config{BaseURL: redir.URL, Version: "test-version",
			Key:   func(context.Context) (string, error) { return canary, nil },
			Retry: RetryPolicy{MaxAttempts: 2, BaseBackoff: time.Millisecond, MaxBackoff: time.Millisecond, MaxRetryAfter: time.Second},
			Sleep: func(_ context.Context, d time.Duration) error { sl = append(sl, d); return nil }, Jitter: func() float64 { return 0 }})
		if err != nil {
			t.Fatal(err)
		}
		_, recs, err := c.Send(context.Background(), body(t), Hooks{})
		redir.Close()
		if err == nil {
			t.Fatalf("%d: a redirect was treated as success", code)
		}
		if len(recs) == 0 || recs[0].Status != code {
			t.Fatalf("%d: attempt records %+v, want the %d itself", code, recs, code)
		}
		if n := hit.Load(); n != 0 {
			t.Fatalf("%d: the redirect target got %d requests, want 0", code, n)
		}
	}
}

// The client's transport never consults the environment proxy, whether the
// caller passed no client, a client with no transport, or one with a
// proxying *http.Transport. A caller's own RoundTripper is kept.
func TestNoEnvProxy(t *testing.T) {
	key := func(context.Context) (string, error) { return canary, nil }
	proxying := &http.Transport{Proxy: http.ProxyFromEnvironment}
	for _, c := range []struct {
		what string
		in   *http.Client
	}{
		{"no client", nil},
		{"client with no transport", &http.Client{}},
		{"client with a proxying transport", &http.Client{Transport: proxying}},
	} {
		cl, err := New(Config{BaseURL: "https://api.anthropic.com", Version: "v", Key: key, HTTP: c.in})
		if err != nil {
			t.Fatal(err)
		}
		tr, ok := cl.cfg.HTTP.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("%s: transport %T, want *http.Transport", c.what, cl.cfg.HTTP.Transport)
		}
		if tr.Proxy != nil {
			t.Fatalf("%s: the transport has a proxy function", c.what)
		}
		if cl.cfg.HTTP.CheckRedirect == nil {
			t.Fatalf("%s: redirects are followed", c.what)
		}
	}
	if proxying.Proxy == nil {
		t.Fatal("the caller's transport was modified in place")
	}
	// Behaviour: with HTTPS_PROXY pointing at a recording proxy, a request
	// to an unresolvable host never reaches the proxy.
	var proxied atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied.Add(1)
		w.WriteHeader(502)
	}))
	defer proxy.Close()
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("NO_PROXY", "")
	var sl []time.Duration
	cl, err := New(Config{BaseURL: "http://fabric-no-proxy-test.invalid", Version: "v", Key: key,
		Retry: RetryPolicy{MaxAttempts: 1, BaseBackoff: time.Millisecond, MaxBackoff: time.Millisecond, MaxRetryAfter: time.Second},
		Sleep: func(_ context.Context, d time.Duration) error { sl = append(sl, d); return nil }, Jitter: func() float64 { return 0 }})
	if err != nil {
		t.Fatal(err)
	}
	// Go caches the environment proxy on first use (sync.Once), so this part
	// only catches a transport built after the env is set; the structural
	// checks above are the main control.
	if _, _, err := cl.Send(context.Background(), body(t), Hooks{}); err == nil {
		t.Fatal("a request to an unresolvable host succeeded")
	}
	if n := proxied.Load(); n != 0 {
		t.Fatalf("the environment proxy received %d requests, want 0", n)
	}
}
