// Package anthropic is a raw HTTP + SSE client for the Anthropic Messages
// API. It uses no vendor SDK so that assistant content (thinking text,
// signatures, unknown fields) stays byte-for-byte what the API produced.
// No model name or price appears in this package: both come from the grant
// and configuration.
package anthropic

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptrace"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// KeySource returns the API key. It is called per attempt so a rotated
// secret is picked up, and its value is never stored on the Client.
type KeySource func(ctx context.Context) (string, error)

// RetryPolicy is the retry matrix's tunables.
type RetryPolicy struct {
	MaxAttempts   int           // total attempts including the first
	BaseBackoff   time.Duration // first backoff when no retry-after
	MaxBackoff    time.Duration
	MaxRetryAfter time.Duration // a longer retry-after parks instead of sleeping
}

func DefaultRetry() RetryPolicy {
	return RetryPolicy{MaxAttempts: 5, BaseBackoff: time.Second, MaxBackoff: 30 * time.Second, MaxRetryAfter: 2 * time.Minute}
}

// Config builds a Client.
type Config struct {
	BaseURL string // e.g. https://api.anthropic.com (config only)
	Version string // anthropic-version header, required, pinned by config
	Key     KeySource
	HTTP    *http.Client
	Retry   RetryPolicy
	SSE     SSELimits
	// Sleep and Jitter exist so tests run on a fake clock. Jitter returns [0,1).
	Sleep  func(context.Context, time.Duration) error
	Jitter func() float64
	// Capture, when set, receives every attempt's raw response for fixture
	// recording. It never receives request headers, so the key cannot reach it.
	Capture func(Capture)
}

// Capture is one attempt as it crossed the wire, minus credentials.
type Capture struct {
	N            int
	RequestBody  []byte
	Status       int
	RequestID    string
	Header       http.Header // allow-listed response headers only
	ResponseBody []byte      // raw SSE or the error JSON
}

var captureHeaders = []string{"Content-Type", "Request-Id", "Retry-After", "Anthropic-Ratelimit-Requests-Remaining"}

type Client struct{ cfg Config }

// ProductionBaseURL is the only Anthropic endpoint the client will use. Tests
// keep this URL and inject a RoundTripper that routes it to a local fake.
const ProductionBaseURL = "https://api.anthropic.com"

func New(cfg Config) (*Client, error) {
	if cfg.BaseURL != ProductionBaseURL {
		return nil, errors.New("anthropic: BaseURL must be exactly " + ProductionBaseURL)
	}
	if cfg.Version == "" || cfg.Key == nil {
		return nil, errors.New("anthropic: Version and Key are required")
	}
	cfg.HTTP = safeHTTP(cfg.HTTP)
	if cfg.Retry.MaxAttempts == 0 {
		cfg.Retry = DefaultRetry()
	}
	if cfg.SSE.MaxBytes == 0 {
		cfg.SSE = DefaultSSELimits()
	}
	if cfg.Sleep == nil {
		cfg.Sleep = func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		}
	}
	if cfg.Jitter == nil {
		cfg.Jitter = rand.Float64
	}
	return &Client{cfg: cfg}, nil
}

// safeHTTP returns a copy of c (or a new client) that never follows a
// redirect and never uses the environment proxy (goodqa#15087 AC-5, red-team
// F14): a redirect or a proxy would carry the x-api-key header somewhere
// other than the configured API host. A redirect comes back as its own 3xx
// response, which the retry matrix treats as a non-2xx answer. A nil or
// *http.Transport transport is replaced by a clone with Proxy nil; any other
// RoundTripper (a test double) is kept as is.
func safeHTTP(c *http.Client) *http.Client {
	var out http.Client
	if c != nil {
		out = *c
	}
	switch t := out.Transport.(type) {
	case nil:
		out.Transport = noProxy(http.DefaultTransport.(*http.Transport))
	case *http.Transport:
		out.Transport = noProxy(t)
	}
	out.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &out
}

func noProxy(t *http.Transport) *http.Transport {
	n := t.Clone()
	n.Proxy = nil
	return n
}

// APIError is a non-2xx response.
type APIError struct {
	Status     int
	Type       string
	Message    string
	RequestID  string
	RetryAfter time.Duration
	HasRetry   bool
}

func (e *APIError) Error() string {
	return fmt.Sprintf("anthropic http %d %s: %s (request-id %s)", e.Status, e.Type, e.Message, e.RequestID)
}

// AttemptRecord is what one HTTP attempt did, for the budget and audit trail.
type AttemptRecord struct {
	N         int
	Status    int    // 0 when no response arrived
	RequestID string // response header, empty when none
	Outcome   string // ok | http_error | stream_error | truncated | transport | protocol
	// MaybeBilled is true when the provider may have processed the request:
	// the connection was established and the body sent. The loop charges the
	// held amount for such an attempt unless usage is known.
	MaybeBilled bool
	Usage       *Usage
	Err         string
}

// Hooks lets the loop hold budget before every attempt (retries included)
// and settle after.
type Hooks struct {
	BeforeAttempt func(n int) error
	AfterAttempt  func(AttemptRecord)
	OnEvent       func(n int) // test seam: called once per stream event
}

var spendLimit = regexp.MustCompile(`(?i)(spend(ing)? limit|usage limit|spend cap|monthly (spend|usage)|credit balance)`)

// RequestSHA is the digest the loop checkpoints for a request body.
func RequestSHA(body []byte) string {
	h := sha256.Sum256(body)
	return hex.EncodeToString(h[:])
}

// Send posts body (already containing "stream":true) and returns the
// assembled message. Retries follow the matrix in AC-4; every attempt calls
// BeforeAttempt first, so a retry is held and counted like the first try.
func (c *Client) Send(ctx context.Context, body []byte, hk Hooks) (*Message, []AttemptRecord, error) {
	var recs []AttemptRecord
	var lastErr error
	for n := 1; n <= c.cfg.Retry.MaxAttempts; n++ {
		if hk.BeforeAttempt != nil {
			if err := hk.BeforeAttempt(n); err != nil {
				return nil, recs, err
			}
		}
		msg, rec, err := c.once(ctx, body, n, hk.OnEvent)
		recs = append(recs, rec)
		if hk.AfterAttempt != nil {
			hk.AfterAttempt(rec)
		}
		if err == nil {
			return msg, recs, nil
		}
		lastErr = err
		wait, retry, perr := c.classify(err, n)
		if perr != nil {
			return nil, recs, perr
		}
		if !retry || ctx.Err() != nil {
			return nil, recs, err
		}
		if n == c.cfg.Retry.MaxAttempts {
			break
		}
		if err := c.cfg.Sleep(ctx, wait); err != nil {
			return nil, recs, err
		}
	}
	return nil, recs, fmt.Errorf("anthropic: %d attempts failed: %w", len(recs), lastErr)
}

// classify decides: park (error), retry after wait, or give up.
func (c *Client) classify(err error, n int) (time.Duration, bool, error) {
	backoff := func() time.Duration {
		d := c.cfg.Retry.BaseBackoff << (n - 1)
		if d <= 0 || d > c.cfg.Retry.MaxBackoff {
			d = c.cfg.Retry.MaxBackoff
		}
		// Full jitter in [d/2, d].
		return d/2 + time.Duration(c.cfg.Jitter()*float64(d/2))
	}
	var ae *APIError
	if errors.As(err, &ae) {
		switch {
		case ae.Status == 429 && !ae.HasRetry:
			return 0, false, &ParkError{Reason: "429 without retry-after: usage tier spend cap reached (" + ae.Message + "); raise the cap or wait for the next period"}
		case ae.Status == 400 && spendLimit.MatchString(ae.Message):
			return 0, false, &ParkError{Reason: "400 spend limit reached: " + ae.Message}
		case ae.Status == 402:
			return 0, false, &ParkError{Reason: "billing error: " + ae.Message}
		case ae.Status == 429 || ae.Status == 529 || ae.Status == 500 || ae.Status == 504 || ae.Status == 502 || ae.Status == 503:
			if ae.HasRetry {
				if ae.RetryAfter > c.cfg.Retry.MaxRetryAfter {
					return 0, false, &ParkError{Reason: fmt.Sprintf("retry-after %s exceeds the %s limit", ae.RetryAfter, c.cfg.Retry.MaxRetryAfter)}
				}
				return ae.RetryAfter, true, nil
			}
			return backoff(), true, nil
		}
		return 0, false, nil
	}
	var se *StreamError
	if errors.As(err, &se) {
		return backoff(), se.Retryable(), nil
	}
	var pe *ProtocolError
	if errors.As(err, &pe) || errors.Is(err, ErrSSELimit) {
		return 0, false, nil
	}
	// Transport failure or truncated stream: nothing was executed, resend.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return 0, false, nil
	}
	return backoff(), true, nil
}

func (c *Client) once(ctx context.Context, body []byte, n int, onEvent func(int)) (*Message, AttemptRecord, error) {
	rec := AttemptRecord{N: n}
	key, err := c.cfg.Key(ctx)
	if err != nil || !apiKeyShape.MatchString(key) {
		rec.Outcome, rec.Err = "transport", "no valid api key available"
		return nil, rec, errors.New("anthropic: valid api key unavailable")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ProductionBaseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		rec.Outcome = "transport"
		return nil, rec, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "text/event-stream")
	req.Header.Set("anthropic-version", c.cfg.Version)
	req.Header.Set("x-api-key", key)
	// MaybeBilled only once the request body has really been written: a dial
	// or TLS failure ran no model, a reset after the write may have.
	wrote := false
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		WroteRequest: func(i httptrace.WroteRequestInfo) { wrote = i.Err == nil },
	}))
	resp, err := c.cfg.HTTP.Do(req)
	rec.MaybeBilled = wrote
	if err != nil {
		rec.Outcome = "transport"
		err = scrubErr(err, key)
		rec.Err = err.Error()
		return nil, rec, err
	}
	defer resp.Body.Close()
	rec.Status = resp.StatusCode
	rec.RequestID = redactSecret(resp.Header.Get("request-id"), key)
	// Keep the successful wire response for credential reflection checks. The
	// same buffer backs optional capture, which is redacted before its callback.
	var raw bytes.Buffer
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.TeeReader(io.LimitReader(resp.Body, 64<<20), &raw), resp.Body}
	if c.cfg.Capture != nil {
		defer func() {
			h := http.Header{}
			for _, k := range captureHeaders {
				if v := redactSecret(resp.Header.Get(k), key); v != "" {
					h.Set(k, v)
				}
			}
			responseBody := []byte(redactSecret(raw.String(), key))
			c.cfg.Capture(Capture{N: n, RequestBody: body, Status: resp.StatusCode, RequestID: rec.RequestID, Header: h, ResponseBody: responseBody})
		}()
	}
	if resp.StatusCode/100 != 2 {
		rec.Outcome = "http_error"
		// 4xx/5xx before a stream: the provider did not run the model.
		rec.MaybeBilled = false
		ae := parseAPIError(resp, key)
		rec.Err = ae.Error()
		return nil, rec, ae
	}
	a := &assembler{}
	rd := newSSEReader(resp.Body, c.cfg.SSE)
	for {
		ev, err := rd.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			rec.Outcome = "truncated"
			if errors.Is(err, ErrSSELimit) {
				rec.Outcome = "protocol"
			}
			err = scrubErr(err, key)
			rec.Err = err.Error()
			if a.msg.Usage.Raw != nil {
				u := a.msg.Usage
				rec.Usage = &u
			}
			return nil, rec, err
		}
		if onEvent != nil {
			onEvent(n)
		}
		done, err := a.feed(ev)
		if err != nil {
			rec.Outcome = "stream_error"
			var pe *ProtocolError
			if errors.As(err, &pe) {
				rec.Outcome = "protocol"
			}
			err = scrubErr(err, key)
			rec.Err = err.Error()
			return nil, rec, err
		}
		if done {
			break
		}
	}
	a.msg.RequestID = rec.RequestID
	u := a.msg.Usage
	rec.Usage = &u
	if key != "" {
		secret := []byte(key)
		reflected := bytes.Contains(raw.Bytes(), secret)
		for _, block := range a.msg.Content {
			if bytes.Contains(block, secret) {
				reflected = true
				break
			}
		}
		if reflected {
			// Never redact or otherwise rewrite signed assistant blocks. Reject the
			// whole response before the adapter or loop can persist any content.
			err := perr("successful response contains the active API key")
			rec.Outcome, rec.Err = "protocol", err.Error()
			return nil, rec, err
		}
	}
	rec.Outcome = "ok"
	return &a.msg, rec, nil
}

func redactSecret(s, key string) string {
	if key == "" {
		return s
	}
	return strings.ReplaceAll(s, key, "[redacted]")
}

// redactedError changes only the displayed text; Unwrap retains the original
// typed error for errors.Is/errors.As and retry classification.
type redactedError struct {
	text  string
	cause error
}

func (e *redactedError) Error() string { return e.text }
func (e *redactedError) Unwrap() error { return e.cause }

func scrubErr(err error, key string) error {
	if key != "" && strings.Contains(err.Error(), key) {
		return &redactedError{text: redactSecret(err.Error(), key), cause: err}
	}
	return err
}

func parseAPIError(resp *http.Response, key string) *APIError {
	ae := &APIError{Status: resp.StatusCode, RequestID: redactSecret(resp.Header.Get("request-id"), key)}
	if v := resp.Header.Get("retry-after"); v != "" {
		if secs, err := strconv.ParseFloat(v, 64); err == nil && secs >= 0 {
			ae.RetryAfter, ae.HasRetry = time.Duration(secs*float64(time.Second)), true
		} else if t, err := http.ParseTime(v); err == nil {
			ae.RetryAfter, ae.HasRetry = max(0, time.Until(t)), true
		}
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var w struct {
		Error struct{ Type, Message string } `json:"error"`
	}
	if json.Unmarshal(b, &w) == nil {
		ae.Type, ae.Message = redactSecret(w.Error.Type, key), redactSecret(w.Error.Message, key)
	}
	if ae.Message == "" {
		ae.Message = "(unparsed error body)"
	}
	if len(ae.Message) > 500 {
		ae.Message = ae.Message[:500]
	}
	ae.Message = redactSecret(ae.Message, key)
	return ae
}
