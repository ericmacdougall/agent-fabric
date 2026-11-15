package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"fabric/internal/credential"
)

// AC-17 against a fake CLI on a real pty: the token lands as
// claude-oauth.v0 with claude-oauth hard-linked to it, and the output
// shows only the URL, the prompt and the length and sha prefix.
func TestCaptureAgainstFakeCLI(t *testing.T) {
	tok := synthToken()
	for name, tc := range map[string]struct {
		tokenLine string
		ok        bool
	}{
		"good":         {tok, true},
		"echoes token": {"export CLAUDE_CODE_OAUTH_TOKEN=" + tok, false},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			script := filepath.Join(root, "claude")
			body := "#!/bin/sh\n[ \"$1\" = setup-token ] || exit 9\ncat <<'X'\n" + strings.ReplaceAll(cliOutput(tok, tc.tokenLine), "\r\n", "\n") + "X\n"
			if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
			secret := filepath.Join(root, "secret")
			os.Mkdir(secret, 0o750)
			p := credential.Policy{Dir: secret, WorkerUID: 999999, WorkerGIDs: []int{999998}, OwnerUID: -1, GroupGID: -1}
			var out bytes.Buffer
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			err := capture(ctx, script, p, nil, &out)
			if strings.Contains(out.String(), tok[13:40]) || (err != nil && strings.Contains(err.Error(), tok[13:40])) {
				t.Fatal("the token reached the agent's output")
			}
			if !tc.ok {
				if err == nil {
					t.Fatal("accepted")
				}
				if ents, _ := os.ReadDir(secret); len(ents) > 1 {
					t.Fatalf("wrote %d files", len(ents))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "captured claude-oauth.v0 len=") || !strings.Contains(out.String(), "sign-in URL:") {
				t.Fatalf("output %q", out.String())
			}
			cur, err := p.LoadCurrent()
			if err != nil || cur.Version != 0 || string(cur.Secret.Value()) != tok {
				t.Fatalf("loader: %v", err)
			}
			for _, n := range []string{"claude-oauth", "claude-oauth.v0"} {
				fi, _ := os.Lstat(filepath.Join(secret, n))
				if fi.Sys().(*syscall.Stat_t).Nlink != 2 || fi.Mode().Perm() != 0o640 {
					t.Fatalf("%s: %v", n, fi.Mode())
				}
			}
		})
	}
}
