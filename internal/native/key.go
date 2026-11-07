package native

import (
	"fmt"
	"sync"
	"time"

	"fabric/internal/policy"
	"fabric/internal/provider/anthropic"
)

// APIBaseURL is the only base_url a grant may name (goodqa#15087 AC-5, main
// WAL w5a7fb2daf13): the key is sent nowhere else. Tests swap the HTTP
// transport through Options, never the grant URL.
const APIBaseURL = "https://api.anthropic.com"

// checkBaseURL refuses anything but exactly APIBaseURL: another scheme,
// host, port, path (a trailing "/" included), query or user info. It runs
// before the key command.
func checkBaseURL(u string) error {
	if u != APIBaseURL {
		return fmt.Errorf("grant refused: base_url must be exactly %s", APIBaseURL)
	}
	return nil
}

// keyRing redacts every key value its source has returned, so a secret
// rotated during a run is scrubbed from tool output as well as the first.
type keyRing struct {
	mu   sync.Mutex
	seen []string
}

func (k *keyRing) add(v string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, s := range k.seen {
		if s == v {
			return
		}
	}
	k.seen = append(k.seen, v)
}

// Redact removes each value seen so far, then the generic secret shapes.
func (k *keyRing) Redact(s string) string {
	k.mu.Lock()
	lits := append([]string(nil), k.seen...)
	k.mu.Unlock()
	return policy.Redactor{Literals: lits}.Redact(s)
}

// newKeyRing is the native key source: the API-key shape check runs in
// anthropic.KeyFromCommand before caching, and every accepted value goes
// into the ring.
func newKeyRing(argv []string, ttl time.Duration) (anthropic.KeySource, *keyRing, error) {
	ring := &keyRing{}
	src, err := anthropic.KeyFromCommand(argv, ttl, ring.add)
	if err != nil {
		return nil, nil, err
	}
	return src, ring, nil
}
