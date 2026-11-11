package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fabric/internal/harness"
	"fabric/internal/harness/harnesstest"
)

func TestDemoBinaryIdentityRejectsTruncationAndOversize(t *testing.T) {
	data := []byte("synthetic executable identity")
	path := filepath.Join(t.TempDir(), "binary")
	if err := os.WriteFile(path, data, 0700); err != nil {
		t.Fatal(err)
	}
	full := sha256.Sum256(data)
	prefix := sha256.Sum256(data[:len(data)-1])
	if err := boundedBinaryIdentity(path, hex.EncodeToString(full[:]), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	if err := boundedBinaryIdentity(path, hex.EncodeToString(prefix[:]), int64(len(data)-1)); err == nil {
		t.Fatal("accepted the digest of a truncated executable")
	}
}

func TestDemoDoctorMetadataFailClosed(t *testing.T) {
	if err := checkClaudeDoctor([]byte("Managed settings (remote): none configured for this organization\n")); err != nil {
		t.Fatal(err)
	}
	skip := []byte("Managed settings (remote): not fetched — requires an Enterprise or Team subscription\n")
	if checkClaudeDoctorForConsumer(skip, false) == nil || checkClaudeDoctorForConsumer(skip, true) != nil {
		t.Fatal("consumer skip lost its account/source gate")
	}
	if !checkClaudeConsumerStatus([]byte(`{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty","subscriptionType":"max"}`)) || checkClaudeConsumerStatus([]byte(`{"loggedIn":true,"authMethod":"api_key","apiProvider":"firstParty","subscriptionType":"max"}`)) {
		t.Fatal("consumer account metadata gate failed")
	}
	if err := checkClaudeDoctor([]byte("Managed settings (remote): No server-managed settings configured\n")); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"", "public-canary", "Managed settings (remote): fetch failed; cached policy applies", "Managed settings (remote): skipped", "Managed settings (remote): loaded", "Managed settings (remote): No policy configured\nManaged settings (remote): No policy configured", "Managed settings (remote): No policy configured public-canary"} {
		if err := checkClaudeDoctor([]byte(s)); err == nil || strings.Contains(err.Error(), "public-canary") {
			t.Fatal("unknown metadata accepted or reflected")
		}
	}
	c := &privateDoctorCapture{}
	if _, err := c.Write(make([]byte, 64<<10)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("public-canary")); err == nil {
		t.Fatal("metadata cap bypassed")
	}
}

func TestDemoPublicLifecycleHasOnlyFixedMetadata(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			var out bytes.Buffer
			rec := &harnesstest.Recorder{}
			s := &demoLifecycleSink{next: rec, out: &out, provider: provider}
			name := "claude-code"
			if provider == "codex" {
				name = "codex"
			}
			for i, k := range []harness.Kind{harness.KindMessage, harness.KindFinal} {
				p := json.RawMessage(`{"role":"assistant","text":"public-canary-prompt-tool-path"}`)
				if k == harness.KindFinal {
					p = json.RawMessage(`{"outcome":"done","exit_code":0}`)
				}
				if err := s.Emit(context.Background(), harness.Event{Version: 1, Harness: name, Session: "owned", Seq: int64(i + 1), Kind: k, Payload: p}); err != nil {
					t.Fatal(err)
				}
			}
			if strings.Contains(out.String(), "public-canary") || strings.Count(out.String(), "mc.event/1") != 2 || !strings.Contains(out.String(), provider+"_demo") {
				t.Fatal("unsafe lifecycle projection")
			}
		})
	}
}

func TestDemoArtifactFailureCannotPublishCompleted(t *testing.T) {
	rec := &harnesstest.Recorder{}
	s := &demoArtifactSink{dir: t.TempDir(), next: rec}
	if err := s.Emit(context.Background(), harness.Event{Kind: harness.KindFinal, Payload: json.RawMessage(`{"outcome":"done","exit_code":0}`)}); err != nil {
		t.Fatal(err)
	}
	var f harness.Final
	if json.Unmarshal(rec.Snapshot()[0].Payload, &f) != nil || f.Outcome != harness.OutcomeFailed {
		t.Fatal("unverified artifact published completed")
	}
}
