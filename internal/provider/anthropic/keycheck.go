package anthropic

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// KeyCheck selects how a key command's output is validated.
//
//   - KeyCheckShape (the default) accepts only the documented API-key shape.
//   - KeyCheckAuthority is an opt-in for a key whose stored shape cannot be
//     predicted: a value that fails the shape check is accepted only if it
//     looks like an Anthropic key (sk-ant- prefix, not an OAuth, refresh,
//     admin or session token) and the Anthropic API itself accepts it (one
//     GET /v1/models answering HTTP 200).
type KeyCheck string

const (
	KeyCheckShape     KeyCheck = "shape"
	KeyCheckAuthority KeyCheck = "authority"
)

// Valid reports whether c is a known mode; the empty string means shape.
func (c KeyCheck) Valid() bool { return c == "" || c == KeyCheckShape || c == KeyCheckAuthority }

// authorityURL is fixed: the key is sent to the production API only.
const authorityURL = ProductionBaseURL + "/v1/models?limit=1"

// headerSafe is checked before a non-shaped value is put in a header: no
// whitespace, control or separator characters, so the value cannot inject a
// header or leave the x-api-key field.
var headerSafe = regexp.MustCompile(`^[A-Za-z0-9._~+/=-]{20,512}$`)

// notAPIKeyPrefixes are Anthropic credentials that are not API keys: OAuth
// access and refresh tokens, admin keys and session tokens. They are refused
// before any request, in any case.
var notAPIKeyPrefixes = []string{"sk-ant-oat", "sk-ant-ort", "sk-ant-admin", "sk-ant-sid"}

// Vouched is the set of key values the authority check accepted. The client's
// own shape guard consults it, so a vouched key can be used while a value
// nobody vouched for still cannot. It stores hashes, never values.
type Vouched struct {
	mu sync.Mutex
	h  map[[32]byte]bool
}

func (v *Vouched) add(k string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.h == nil {
		v.h = map[[32]byte]bool{}
	}
	v.h[sha256.Sum256([]byte(k))] = true
}

// Has reports whether k was vouched for. A nil Vouched vouches for nothing.
func (v *Vouched) Has(k string) bool {
	if v == nil {
		return false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.h[sha256.Sum256([]byte(k))]
}

// KeyOptions adds opt-in behaviour to KeyFromCommandWith.
type KeyOptions struct {
	Check KeyCheck
	// HTTP is used for the authority request (tests inject a fake transport).
	// It is wrapped like the main client's: no redirects, no environment proxy.
	HTTP *http.Client
	// OnCheck, when set, sees each authority request's HTTP status and the
	// first 8 hex characters of the key's sha256, never the value.
	OnCheck func(status int, sha256Prefix string)
	// Vouched, when set, receives every value the authority check accepted.
	Vouched *Vouched
}

// checkAuthority returns the HTTP status of one GET /v1/models with the key,
// and 0 with an error when there was no response.
func checkAuthority(ctx context.Context, hc *http.Client, key string) (int, error) {
	c := safeHTTP(hc)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, authorityURL, nil)
	if err != nil {
		return 0, fmt.Errorf("request")
	}
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := c.Do(req)
	if err != nil {
		// err can name the URL but never the key; keep it generic anyway.
		return 0, fmt.Errorf("no response")
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode, nil
}

// authorityAccepts decides a value that failed the shape check. knownGood is
// true when this exact value was accepted before: a temporary outage (no
// response, 429 or 5xx) then keeps it, while an outage for a new value, and
// any other non-200 answer, refuses it.
func authorityAccepts(ctx context.Context, argv0, k string, o KeyOptions, knownGood bool) error {
	low := strings.ToLower(k)
	if o.Check != KeyCheckAuthority || !strings.HasPrefix(low, "sk-ant-") || !headerSafe.MatchString(k) {
		return fmt.Errorf("anthropic: key command %q %w", argv0, ErrKeyShape)
	}
	for _, p := range notAPIKeyPrefixes {
		if strings.HasPrefix(low, p) {
			return fmt.Errorf("anthropic: key command %q %w", argv0, ErrKeyShape)
		}
	}
	status, err := checkAuthority(ctx, o.HTTP, k)
	if o.OnCheck != nil {
		sum := sha256.Sum256([]byte(k))
		o.OnCheck(status, hex.EncodeToString(sum[:])[:8])
	}
	transient := err != nil || status == http.StatusTooManyRequests || status >= 500
	if status == http.StatusOK || (transient && knownGood) {
		if o.Vouched != nil {
			o.Vouched.add(k)
		}
		return nil
	}
	return fmt.Errorf("anthropic: key command %q: key authority check refused the value (HTTP %d)", argv0, status)
}
