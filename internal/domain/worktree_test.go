package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// goodqa#15283 AC-9: a stored pre-0038 ATTEMPT_STARTED event and START_ATTEMPT
// command, which carry no worktree, keep the exact bytes and hashes the
// cf50e55 binary produced. The constants were computed by that binary.
func TestAttemptStartedWithoutWorktreeKeepsCf50e55Bytes(t *testing.T) {
	e := Event{Kind: EvAttemptStarted, Mission: "m_gold", Seq: 2, Attempt: "r_gold", Fence: 1, Host: "h_t2", Role: "writer",
		Path: ProviderAnthropicAPI, LockPath: "/s/native-locks/r_gold.lock", AttemptTo: AttemptLeased}
	const wantEvent = `{"kind":"ATTEMPT_STARTED","mission":"m_gold","seq":2,"attempt":"r_gold","fence":1,"host":"h_t2","role":"writer","path":"anthropic-api","lock_path":"/s/native-locks/r_gold.lock","attempt_to":"LEASED"}`
	const wantEventSHA = "77b834078a82e0dde1fb2a435f95ae4432a4fd613506c42874fe66fa2bf7baef"
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	if string(b) != wantEvent || hex.EncodeToString(sum[:]) != wantEventSHA {
		t.Fatalf("event bytes changed:\n got %s (%x)\nwant %s (%s)", b, sum, wantEvent, wantEventSHA)
	}
	var back Event
	if err := json.Unmarshal([]byte(wantEvent), &back); err != nil || back.Worktree != "" {
		t.Fatalf("stored event decodes to worktree %q (%v), want none", back.Worktree, err)
	}

	c := Command{Kind: CmdStartAttempt, Mission: "m_gold", Attempt: "r_gold", Host: "h_t2", Role: "writer",
		Path: ProviderAnthropicAPI, LockPath: "/s/native-locks/r_gold.lock"}
	const wantCmd = `{"kind":"START_ATTEMPT","mission":"m_gold","attempt":"r_gold","host":"h_t2","role":"writer","path":"anthropic-api","lock_path":"/s/native-locks/r_gold.lock"}`
	const wantCmdSHA = "adbcc68cdc1d8663a36cdd3c226efd50c16b054183b897378c8ff0f483f5ce86"
	if got := string(c.CanonicalRequest()); got != wantCmd || c.RequestHash() != wantCmdSHA {
		t.Fatalf("command bytes changed:\n got %s (%s)\nwant %s (%s)", got, c.RequestHash(), wantCmd, wantCmdSHA)
	}

	// A bound start does carry the field (so the control above is not vacuous).
	c.Worktree, e.Worktree = "/srv/wt", "/srv/wt"
	if c.RequestHash() == wantCmdSHA || !strings.Contains(string(c.CanonicalRequest()), `"worktree":"/srv/wt"`) {
		t.Fatal("a command with a worktree hashes like one without")
	}
	if b, _ := json.Marshal(e); !strings.Contains(string(b), `"worktree":"/srv/wt"`) {
		t.Fatalf("event with a worktree does not record it: %s", b)
	}
}

// ValidWorktree is the Go half of attempts_worktree_check; the shared vectors
// against the database are internal/store TestAttemptsWorktreeConstraints.
// These are the Go-only refusals and the case rule (NEG-6).
func TestValidWorktreeGoOnly(t *testing.T) {
	for _, p := range []string{
		"/srv/\xffwt",   // invalid UTF-8
		"/srv/w\u200bt", // zero-width space (Cf)
		"/srv/w\u202et", // right-to-left override (bidi, Cf)
		"/srv/w\u2066t", // left-to-right isolate (bidi)
		"/srv/w\u061ct", // Arabic letter mark (bidi)
		"/srv/w\u0085t", // C1 control (NEL)
		"/srv/w\ufefft", // byte order mark (Cf)
		"/srv/wt\x00",   // NUL
		"/" + strings.Repeat("a", MaxWorktreeBytes), // 4097 bytes
	} {
		if err := ValidWorktree(p); !errors.Is(err, ErrInvalidID) {
			t.Errorf("ValidWorktree(%q) = %v, want ErrInvalidID", p, err)
		}
	}
	for _, p := range []string{"/srv/wt", "/srv/WT", "/srv/Wt \u00e9", "/" + strings.Repeat("a", MaxWorktreeBytes-1)} {
		if err := ValidWorktree(p); err != nil {
			t.Errorf("ValidWorktree(%q) = %v, want nil", p, err)
		}
	}
}
