package anthropic

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"fabric/internal/provider/anthropic/fakeapi"
)

// A value the authority check accepted keeps working through a temporary
// outage (429, 5xx) of the check, but a definite refusal is never overridden.
func TestKeyCheckOutageKeepsOnlyAVouchedValue(t *testing.T) {
	odd := "sk-ant-zzz-" + strings.Repeat("aB3_-", 18)
	run := func(second int) error {
		tr := &authTransport{status: 200}
		src, _ := KeyFromCommandWith([]string{"printf", "%s", odd}, time.Nanosecond, nil,
			KeyOptions{Check: KeyCheckAuthority, HTTP: &http.Client{Transport: tr}, Vouched: &Vouched{}})
		if _, err := src(context.Background()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond) // the nanosecond ttl expires: the next call refetches
		tr.status = second
		_, err := src(context.Background())
		return err
	}
	for _, status := range []int{429, 500, 503} {
		if err := run(status); err != nil {
			t.Fatalf("status %d: a vouched value was dropped: %v", status, err)
		}
	}
	for _, status := range []int{401, 403, 404} {
		if err := run(status); err == nil {
			t.Fatalf("status %d was overridden by an earlier acceptance", status)
		}
	}
}

// The authority request never uses the environment proxy: the key goes to the
// API host only.
func TestKeyCheckIgnoresEnvironmentProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("https_proxy", "http://127.0.0.1:1")
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	var dialed []string
	hc := &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: func(_ context.Context, _, addr string) (net.Conn, error) {
			dialed = append(dialed, addr)
			return nil, errors.New("blocked in test")
		},
	}}
	if _, err := checkAuthority(context.Background(), hc, "sk-ant-zzz-"+strings.Repeat("aB3_-", 18)); err == nil {
		t.Fatal("unexpected success")
	}
	if len(dialed) != 1 || dialed[0] != "api.anthropic.com:443" {
		t.Fatalf("dialed %v, want only api.anthropic.com:443 (the proxy must be ignored)", dialed)
	}
}

// An authority-accepted key is usable by the client (not only by the key
// source) and is seen by onKey for redaction; an odd value nobody vouched for
// is still refused by the client's own guard.
func TestAuthorityKeyDrivesTheClient(t *testing.T) {
	odd := "sk-ant-zzz-" + strings.Repeat("aB3_-", 18)
	var seen []string
	v := &Vouched{}
	src, _ := KeyFromCommandWith([]string{"printf", "%s", odd}, time.Minute, func(k string) { seen = append(seen, k) },
		KeyOptions{Check: KeyCheckAuthority, HTTP: &http.Client{Transport: &authTransport{status: 200}}, Vouched: v})
	send := func(key KeySource, vouched *Vouched) error {
		s := fakeapi.New(ok("hello"))
		defer s.Close()
		c, err := New(Config{BaseURL: ProductionBaseURL, HTTP: s.HTTPClient(), Version: "v", Key: key, Vouched: vouched, Retry: RetryPolicy{MaxAttempts: 1}})
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = c.Send(context.Background(), body(t), Hooks{})
		return err
	}
	if err := send(src, v); err != nil {
		t.Fatalf("an authority-accepted key could not send: %v", err)
	}
	if len(seen) != 1 || seen[0] != odd {
		t.Fatalf("onKey saw %d values", len(seen))
	}
	plain := func(context.Context) (string, error) { return odd, nil }
	if err := send(plain, &Vouched{}); err == nil {
		t.Fatal("an unvouched odd value reached the API")
	}
}
