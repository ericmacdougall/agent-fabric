package credential

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// goodqa#15966 AC-18 NEG: a credential rotated during the attempt (or any
// other settle-loader failure) submits nothing and publishes no stream.
func TestSettleSubmitFailsClosed(t *testing.T) {
	setup := func(t *testing.T, tok []byte) (fixture, []byte, []OtherCredential, string, string, []byte) {
		f := newFixture(t)
		if tok == nil {
			tok = synthToken(t, 108)
		}
		_, err := f.p.Capture(tok)
		must(t, err)
		key, _ := LoadIDKey(f.state)
		oth := others(t)
		_, err = f.p.PrepareLaunch(f.bindings, key, "m1", "att-9", oth)
		must(t, err)
		raw := t.TempDir()
		must(t, os.WriteFile(filepath.Join(raw, "stream.jsonl"), jsonLine("leak "+string(tok)), 0o600))
		must(t, os.WriteFile(filepath.Join(raw, "stderr.log"), []byte("ok\n"), 0o600))
		return f, key, oth, raw, t.TempDir(), tok
	}
	run := func(f fixture, key []byte, now []OtherCredential, raw, out string) (int, error) {
		calls := 0
		_, err := f.p.SettleSubmit(f.bindings, "att-9", key, now,
			filepath.Join(raw, "stream.jsonl"), filepath.Join(raw, "stderr.log"), out,
			func(*Bound, RedactResult) error { calls++; return nil })
		return calls, err
	}

	t.Run("clean settle submits once with the token redacted", func(t *testing.T) {
		f, key, oth, raw, out, tok := setup(t, nil)
		calls, err := run(f, key, oth, raw, out)
		if err != nil || calls != 1 {
			t.Fatalf("calls=%d err=%v", calls, err)
		}
		b, _ := os.ReadFile(filepath.Join(out, "stream.jsonl"))
		if len(b) == 0 || containsBytes(b, tok[13:40]) {
			t.Fatal("redacted stream missing or still carries the token")
		}
	})
	t.Run("rotated mid attempt: nothing submitted, nothing published", func(t *testing.T) {
		f, key, oth, raw, out, _ := setup(t, nil)
		now := append([]OtherCredential(nil), oth...)
		now[0].Value = synthSecret(t, 40)
		calls, err := run(f, key, now, raw, out)
		if calls != 0 || settleClass(err) != ClassRotatedMidAttempt {
			t.Fatalf("calls=%d err=%v", calls, err)
		}
		if ents, _ := os.ReadDir(out); len(ents) != 0 {
			t.Fatalf("%d files published after a fail-closed settle", len(ents))
		}
	})
	t.Run("material unavailable: nothing submitted", func(t *testing.T) {
		f, key, oth, raw, out, _ := setup(t, nil)
		must(t, os.Remove(filepath.Join(f.bindings, "att-9.json")))
		calls, err := run(f, key, oth, raw, out)
		if calls != 0 || settleClass(err) != ClassMaterialUnavailable {
			t.Fatalf("calls=%d err=%v", calls, err)
		}
	})
	t.Run("blocked delivery: nothing submitted", func(t *testing.T) {
		// A token carrying a digit run: that run inside a JSON number cannot
		// be rewritten in place, so delivery blocks.
		tok := append(synthToken(t, 50), []byte("9081726354")...)
		tok = append(tok, synthToken(t, 60)[11:]...)
		f, key, oth, raw, out, _ := setup(t, tok)
		must(t, os.WriteFile(filepath.Join(raw, "stream.jsonl"), []byte(`{"type":"result","n":9081726354,"s":"ok"}`+"\n"), 0o600))
		calls, err := run(f, key, oth, raw, out)
		if calls != 0 || !errors.Is(err, ErrDeliveryBlocked) {
			t.Fatalf("calls=%d err=%v", calls, err)
		}
		if ents, _ := os.ReadDir(out); len(ents) != 0 {
			t.Fatalf("%d redacted copies published on a blocked delivery", len(ents))
		}
	})
}

func containsBytes(b, sub []byte) bool {
	return len(sub) > 0 && len(b) >= len(sub) && string(b) != "" && (func() bool {
		for i := 0; i+len(sub) <= len(b); i++ {
			if string(b[i:i+len(sub)]) == string(sub) {
				return true
			}
		}
		return false
	})()
}
