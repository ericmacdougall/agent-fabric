package anthropic

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// KeyFromCommand returns a KeySource that runs argv (for example
// `gcloud secrets versions access latest --secret NAME`) and uses its trimmed
// stdout as the key. The key never appears in argv, config or this
// process's environment variables, and errors carry the command name and
// exit status only, never its output. Results are cached for ttl so a long
// mission does not spawn a process per request while a rotated secret is
// still picked up.
func KeyFromCommand(argv []string, ttl time.Duration) (KeySource, error) {
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
		cached, at = k, time.Now()
		return k, nil
	}, nil
}
