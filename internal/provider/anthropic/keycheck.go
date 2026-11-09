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
	"time"
)

// KeyCheck selects how a key command's output is validated.
//
//   - KeyCheckShape (the default) accepts only the documented API-key shape.
//   - KeyCheckAuthority is an opt-in for a key whose stored shape cannot be
//     predicted: a value that fails the shape check is accepted only if the
//     Anthropic API itself accepts it (one GET /v1/models answering HTTP
//     200). An OAuth token (sk-ant-oat...) is refused before any request.
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

// KeyOptions adds opt-in behaviour to KeyFromCommandWith.
type KeyOptions struct {
	Check KeyCheck
	// HTTP is used for the authority request (tests inject a fake transport).
	// Its redirects are never followed, so the key cannot leave the API host.
	HTTP *http.Client
	// OnCheck, when set, sees each authority request's HTTP status and the
	// first 8 hex characters of the key's sha256, never the value.
	OnCheck func(status int, sha256Prefix string)
}

// checkAuthority returns the HTTP status of one GET /v1/models with the key.
func checkAuthority(ctx context.Context, hc *http.Client, key string) (int, error) {
	if hc == nil {
		hc = &http.Client{}
	}
	c := *hc
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
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

// authorityAccepts decides a value that failed the shape check.
func authorityAccepts(ctx context.Context, argv0, k string, o KeyOptions) error {
	if o.Check != KeyCheckAuthority || strings.HasPrefix(strings.ToLower(k), "sk-ant-oat") || !headerSafe.MatchString(k) {
		return fmt.Errorf("anthropic: key command %q %w", argv0, ErrKeyShape)
	}
	status, err := checkAuthority(ctx, o.HTTP, k)
	if o.OnCheck != nil {
		sum := sha256.Sum256([]byte(k))
		o.OnCheck(status, hex.EncodeToString(sum[:])[:8])
	}
	if err != nil || status != http.StatusOK {
		return fmt.Errorf("anthropic: key command %q: key authority check refused the value (HTTP %d)", argv0, status)
	}
	return nil
}
