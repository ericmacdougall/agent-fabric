package sched

import (
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"fabric/internal/credential"
	"fabric/internal/domain"
	"fabric/internal/storetest"
)

func synthTok(n int) []byte {
	const alpha = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	b := make([]byte, n)
	rand.Read(b)
	out := []byte("sk-" + "ant-oat01-")
	for i := 0; len(out) < n; i++ {
		out = append(out, alpha[int(b[i])&63])
	}
	return out
}

func waitStarts(t *testing.T, ln *fakeLauncher, n int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt32(&ln.starts) < n {
		if time.Now().After(deadline) {
			t.Fatal("never launched")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// goodqa#15966 AC-14, AC-15, AC-18 through the real admission path.
func TestCredentialLaunchAndStreamRoot(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	secret := filepath.Join(root, "secret")
	bindings := filepath.Join(root, "bindings")
	streams := filepath.Join(root, "streams")
	for _, d := range []string{secret, bindings} {
		if err := os.Mkdir(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	pol := credential.Policy{Dir: secret, WorkerUID: 999999, WorkerGIDs: []int{999998}, OwnerUID: -1, GroupGID: -1}
	tok := synthTok(108)
	if _, err := pol.Capture(tok); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	pin, _ := credential.FileSHA256(bin)

	launch := func(t *testing.T, cred *CredentialLaunch) (*fakeLauncher, string) {
		st := storetest.Open(t)
		setCap(t, st, 5)
		admitSpec(t, st, "m_w3", domain.ProviderClaudeCode, 1)
		runner := storetest.RunnerDSN(t, st)
		ln := &fakeLauncher{st: st, block: make(chan struct{})}
		t.Cleanup(func() { close(ln.block) })
		work := t.TempDir()
		cfg := Config{Host: "h_t2", Model: "fake-model", Effort: "n/a", Rung: "R0", TierTableSHA: "test", Profile: "P1", MaxTurns: 5,
			MaxBudgetUSD: 0.01, Wall: time.Minute, WorkRoot: work, RunnerDSN: runner, StreamRoot: streams, Credential: cred}
		cfg.Launch.Binary = bin
		s := New(st, cfg, ln)
		if err := s.runOnce(ctx, "m_w3"); err != nil {
			t.Fatal(err)
		}
		return ln, work
	}

	t.Run("token launch: binding first, claude env, streams in the fabd-only root", func(t *testing.T) {
		ln, work := launch(t, &CredentialLaunch{Policy: pol, BindingsDir: bindings, IDKey: make([]byte, 32), PinnedSHA: pin,
			Others: func() []credential.OtherCredential {
				return []credential.OtherCredential{{Kind: credential.KindGoodQA, Name: "goodqa", Value: []byte("synthetic-goodqa-key-0123456789")}}
			}})
		waitStarts(t, ln, 1)
		ln.mu.Lock()
		env := ln.envs[0]
		ln.mu.Unlock()
		names := map[string]string{}
		for _, e := range env {
			n, v, _ := strings.Cut(e, "=")
			names[n] = v
			if strings.Contains(v, streams) {
				t.Fatalf("worker variable %s names the fabd-only stream root", n)
			}
		}
		for _, bad := range []string{"FABRIC_DSN", "FABRIC_HOME", "ANTHROPIC_API_KEY"} {
			if _, ok := names[bad]; ok {
				t.Fatalf("%s reached the claude-code worker", bad)
			}
		}
		if names["CLAUDE_CODE_OAUTH_TOKEN"] != string(tok) || names["CLAUDE_CODE_SUBPROCESS_ENV_SCRUB"] != "1" ||
			!strings.HasPrefix(names["HOME"], work) {
			t.Fatal("claude-code environment incomplete")
		}
		ents, _ := os.ReadDir(bindings)
		found := false
		for _, e := range ents {
			if strings.HasSuffix(e.Name(), ".json") {
				found = true
			}
		}
		if !found {
			t.Fatal("no binding record")
		}
		sd, _ := os.ReadDir(streams)
		if len(sd) != 1 {
			t.Fatalf("%d attempt stream dirs, want 1", len(sd))
		}
		for _, f := range []string{"stream.jsonl", "stderr.log"} {
			fi, err := os.Lstat(filepath.Join(streams, sd[0].Name(), f))
			if err != nil || fi.Mode().Perm() != 0o600 {
				t.Fatalf("%s: %v", f, err)
			}
			if _, err := os.Lstat(filepath.Join(work, "run", sd[0].Name(), f)); err == nil {
				t.Fatalf("%s also in the worker run dir", f)
			}
		}
		if fi, _ := os.Lstat(filepath.Join(streams, sd[0].Name())); fi.Mode().Perm() != 0o700 {
			t.Fatalf("stream dir mode %v", fi.Mode())
		}
	})

	t.Run("NEG-11: a binary that is not the pinned claude starts nothing", func(t *testing.T) {
		ln, _ := launch(t, &CredentialLaunch{Policy: pol, BindingsDir: bindings, IDKey: make([]byte, 32), PinnedSHA: strings.Repeat("0", 64)})
		time.Sleep(200 * time.Millisecond)
		if n := atomic.LoadInt32(&ln.starts); n != 0 {
			t.Fatalf("%d starts with a mismatched pin", n)
		}
	})

	t.Run("NEG-24: a binding record that cannot be written starts nothing", func(t *testing.T) {
		ln, _ := launch(t, &CredentialLaunch{Policy: pol, BindingsDir: filepath.Join(root, "missing"), IDKey: make([]byte, 32), PinnedSHA: pin})
		time.Sleep(200 * time.Millisecond)
		if n := atomic.LoadInt32(&ln.starts); n != 0 {
			t.Fatalf("%d starts without a binding record", n)
		}
	})
}
