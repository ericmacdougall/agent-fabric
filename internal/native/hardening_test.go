package native_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"fabric/internal/agent"
	"fabric/internal/domain"
	"fabric/internal/native"
	"fabric/internal/provider/anthropic/fakeapi"
	"fabric/internal/store"
	"fabric/internal/tools"
	"fabric/internal/tools/fakehost"
)

type expiringLeaseTransport struct {
	st      *store.Store
	attempt string
}

func (f *expiringLeaseTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := context.Background()
	var initial time.Time
	if err := f.st.Pool().QueryRow(ctx, `SELECT lease_until FROM attempts WHERE id = $1`, f.attempt).Scan(&initial); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		var current time.Time
		if err := f.st.Pool().QueryRow(ctx, `SELECT lease_until FROM attempts WHERE id = $1`, f.attempt).Scan(&current); err != nil {
			return nil, err
		}
		if current.After(initial) {
			break
		}
		if time.Now().After(deadline) {
			return &http.Response{StatusCode: 400, Header: make(http.Header), Request: req,
				Body: io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"invalid_request_error","message":"heartbeat did not renew"}}`))}, nil
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := f.st.Pool().Exec(ctx, `UPDATE attempts SET lease_until = now() - interval '1 second' WHERE id = $1`, f.attempt); err != nil {
		return nil, err
	}
	<-req.Context().Done()
	return nil, req.Context().Err()
}

func TestHeartbeatCancelsBlockedProviderWhenLeaseExpires(t *testing.T) {
	st := openNativeStore(t)
	st.Lease = 120 * time.Millisecond
	e := newEnv(t)
	o := native.Options{
		Starter:  fakehost.New(),
		Boundary: tools.Boundary{Host: "fake", Contained: true},
		HTTP:     &http.Client{Transport: &expiringLeaseTransport{st: st, attempt: e.g.Attempt}},
	}
	runCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	_, err := native.Run(runCtx, st, e.g, o)
	if !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("blocked provider error = %v, want expired-lease ErrInvalidTransition", err)
	}

	// A leaked heartbeat would renew this restored lease. A correctly stopped
	// heartbeat leaves it untouched after Run has returned.
	ctx := context.Background()
	if _, err := st.Pool().Exec(ctx, `UPDATE attempts SET lease_until = now() + interval '1 hour' WHERE id = $1`, e.g.Attempt); err != nil {
		t.Fatal(err)
	}
	var before time.Time
	if err := st.Pool().QueryRow(ctx, `SELECT lease_until FROM attempts WHERE id = $1`, e.g.Attempt).Scan(&before); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * st.Lease / 2)
	var after time.Time
	if err := st.Pool().QueryRow(ctx, `SELECT lease_until FROM attempts WHERE id = $1`, e.g.Attempt).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !after.Equal(before) {
		t.Fatalf("heartbeat survived Run: lease moved from %s to %s", before, after)
	}
}

func TestLeaseExpiryAfterResponsePreventsTool(t *testing.T) {
	st := openNativeStore(t)
	e := newEnv(t, fakeapi.Step{Events: fakeapi.ToolUse("", "", "toolu_x", "shell", `{"command":"echo should-not-run"}`, 1, 1)})
	e.srv.Validate = func([]byte) error {
		_, err := st.Pool().Exec(context.Background(), `UPDATE attempts SET lease_until = now() - interval '1 second' WHERE id = $1`, e.g.Attempt)
		return err
	}
	host := fakehost.New()
	o := native.Options{Starter: host, Boundary: tools.Boundary{Host: "fake", Contained: true}, HTTP: e.srv.HTTPClient()}
	_, err := native.Run(context.Background(), st, e.g, o)
	if !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("expired lease error = %v", err)
	}
	if len(host.Starts) != 0 {
		t.Fatalf("expired attempt launched %d tools", len(host.Starts))
	}
}

func TestMonthResourceChangesAtUTCRollover(t *testing.T) {
	st := openNativeStore(t)
	month := "202601"
	preSpend(t, st, agent.SpendResource("month", "202602"), 19_999_950)
	defer native.SetMonthSource(func(context.Context, *store.Store) (string, error) { return month, nil })()
	e := newEnv(t,
		fakeapi.Step{Events: fakeapi.ToolUse("", "", "toolu_r", "read", `{"path":"notes.txt"}`, 1, 1)},
		fakeapi.Step{Events: fakeapi.Text("must not be requested", 1, 1)},
	)
	e.srv.Validate = func([]byte) error {
		month = "202602"
		return nil
	}
	r, err := native.Run(context.Background(), st, e.g, opts(true))
	if err != nil {
		t.Fatal(err)
	}
	if r.Outcome != agent.OutcomeBudgetExhausted || len(e.srv.Requests()) != 1 {
		t.Fatalf("rollover outcome=%q requests=%d", r.Outcome, len(e.srv.Requests()))
	}
}

func TestMonthResourceChangesBeforeHTTPRetry(t *testing.T) {
	st := openNativeStore(t)
	month := "202601"
	preSpend(t, st, agent.SpendResource("month", "202602"), 19_999_950)
	defer native.SetMonthSource(func(context.Context, *store.Store) (string, error) { return month, nil })()
	e := newEnv(t,
		fakeapi.Step{Status: 500, ErrType: "api_error", ErrMessage: "retry"},
		fakeapi.Step{Events: fakeapi.Text("must not be requested", 1, 1)},
	)
	e.srv.Validate = func([]byte) error {
		month = "202602"
		return nil
	}
	r, err := native.Run(context.Background(), st, e.g, opts(true))
	if err != nil {
		t.Fatal(err)
	}
	if r.Outcome != agent.OutcomeBudgetExhausted || len(e.srv.Requests()) != 1 {
		t.Fatalf("retry rollover outcome=%q requests=%d", r.Outcome, len(e.srv.Requests()))
	}
}
