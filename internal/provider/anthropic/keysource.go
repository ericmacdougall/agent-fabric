package anthropic

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// apiKeyShape is the only key shape the native loop accepts (goodqa#15087
// AC-5): an Anthropic API key. OAuth (sk-ant-oat01-), admin and other
// tokens are refused, so a subscription login can never drive this client.
var apiKeyShape = regexp.MustCompile(`^sk-ant-api[0-9]{2}-[A-Za-z0-9_-]{80,200}$`)

// ErrKeyShape is a key command output that is not an API key. Its text
// names the expected shape, never the value.
var ErrKeyShape = errors.New("returned a value that is not an Anthropic API key (want sk-ant-apiNN- followed by 80 to 200 characters of A-Z a-z 0-9 _ -)")

// KeyFromCommand returns a KeySource that runs argv (for example
// `gcloud secrets versions access latest --secret NAME`) and uses its trimmed
// stdout as the key. The key never appears in argv, config or this
// process's environment variables, and errors carry the command name and
// exit status only, never its output. The value must have the API-key
// shape; that check runs before caching, so a refused value is never
// cached and the next call runs the command again. Accepted values are
// cached for ttl so a long mission does not spawn a process per request
// while a rotated secret is still picked up. onKey, when set, sees every
// accepted value (once per fetch), so the caller can redact each of them,
// including a rotated one.
func KeyFromCommand(argv []string, ttl time.Duration, onKey func(string)) (KeySource, error) {
	if len(argv) == 0 {
		return nil, errors.New("anthropic: key command is empty")
	}
	var mu sync.Mutex
	var cached string
	var at time.Time
	return func(ctx context.Context) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if cached != "" && time.Since(at) < ttl {
			return cached, nil
		}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
		var out bytes.Buffer
		cmd.Stdout = &out
		// stderr is dropped: a secret manager may echo identifiers we do not want in logs.
		if err := cmd.Run(); err != nil {
			code := -1
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code = ee.ExitCode()
			}
			return "", fmt.Errorf("anthropic: key command %q failed (exit %d)", argv[0], code)
		}
		k := strings.TrimSpace(out.String())
		if k == "" {
			return "", fmt.Errorf("anthropic: key command %q returned nothing", argv[0])
		}
		if strings.ContainsAny(k, " \t\r\n") {
			return "", fmt.Errorf("anthropic: key command %q returned more than one token", argv[0])
		}
		if !apiKeyShape.MatchString(k) {
			return "", fmt.Errorf("anthropic: key command %q %w", argv[0], ErrKeyShape)
		}
		if onKey != nil {
			onKey(k)
		}
		cached, at = k, time.Now()
		return k, nil
	}, nil
}
