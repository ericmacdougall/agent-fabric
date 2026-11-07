package native_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"fabric/internal/domain"
	"fabric/internal/native"
	"fabric/internal/provider/anthropic/fakeapi"
	"fabric/internal/store"
	"fabric/internal/storetest"
)

// goodqa#15087 AC-1: the native runner creates anthropic-api missions only,
// and its attempt is stored with path anthropic-api and its own lock path.
// An existing claude-code mission of the same id is refused and keeps its
// provider.
func TestNativeCreatesAnthropicAPIOnly(t *testing.T) {
	ctx := context.Background()
	stateDir(t)
	t.Run("a new mission is anthropic-api", func(t *testing.T) {
		st := storetest.Open(t)
		e := newEnv(t, fakeapi.Step{Events: fakeapi.Text("finished", 50, 5)})
		if r, err := native.Run(ctx, st, e.g, opts(true)); err != nil || r.Outcome != "done" {
			t.Fatalf("run: %+v %v", r, err)
		}
		var provider, path, lock string
		if err := st.Pool().QueryRow(ctx, `SELECT m.provider, a.path, coalesce(a.lock_path, '') FROM missions m JOIN attempts a ON a.mission_id = m.id
			WHERE m.id = $1`, e.g.Mission).Scan(&provider, &path, &lock); err != nil {
			t.Fatal(err)
		}
		want, err := native.LockPath(domain.AttemptID(e.g.Attempt))
		if err != nil {
			t.Fatal(err)
		}
		if provider != string(domain.ProviderAnthropicAPI) || path != string(domain.ProviderAnthropicAPI) || lock != want {
			t.Fatalf("mission provider %q, attempt path %q, lock %q; want anthropic-api, anthropic-api, %q", provider, path, lock, want)
		}
	})
	t.Run("an existing claude-code mission is refused", func(t *testing.T) {
		st := storetest.Open(t)
		e := newEnv(t, fakeapi.Step{Events: fakeapi.Text("never", 50, 5)})
		if err := st.CreateMission(ctx, domain.MissionID(e.g.Mission), domain.ProviderClaudeCode, e.g.Ticket, "", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := native.Run(ctx, st, e.g, opts(true)); err == nil {
			t.Fatal("native run on a claude-code mission succeeded")
		}
		var p string
		if err := st.Pool().QueryRow(ctx, "SELECT provider FROM missions WHERE id = $1", e.g.Mission).Scan(&p); err != nil || p != string(domain.ProviderClaudeCode) {
			t.Fatalf("provider %q after the refusal, want claude-code", p)
		}
		if n := count(t, st, "SELECT count(*) FROM attempts WHERE mission_id = $1", e.g.Mission); n != 0 || len(e.srv.Requests()) != 0 {
			t.Fatalf("%d attempts, %d requests; want none", n, len(e.srv.Requests()))
		}
	})
}

// goodqa#15087 AC-2: with native's own provider checks turned off, a native
// start on an admitted claude-code mission is still refused, by the reducer
// or, failing that, by the database trigger. Both name the path mismatch.
// No attempt row is written and no request is sent.
func TestNativePathDefenseInDepth(t *testing.T) {
	ctx := context.Background()
	stateDir(t)
	setup := func(t *testing.T) (*store.Store, *env) {
		st := storetest.Open(t)
		e := newEnv(t, fakeapi.Step{Events: fakeapi.Text("never", 50, 5)})
		m := domain.MissionID(e.g.Mission)
		if err := st.CreateMission(ctx, m, domain.ProviderClaudeCode, e.g.Ticket, "", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Execute(ctx, "chat", "admit-dd", domain.Command{Kind: domain.CmdAdmitMission, Mission: m}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if n := count(t, st, "SELECT count(*) FROM attempts WHERE mission_id = $1", e.g.Mission); n != 0 {
				t.Errorf("%d attempt rows for the claude-code mission, want 0", n)
			}
			if len(e.srv.Requests()) != 0 {
				t.Errorf("%d requests sent, want 0", len(e.srv.Requests()))
			}
		})
		return st, e
	}
	t.Run("pre-check on: refused by native first", func(t *testing.T) { // control
		st, e := setup(t)
		if _, err := native.Run(ctx, st, e.g, opts(true)); !errors.Is(err, native.ErrStartRefused) {
			t.Fatalf("run: %v, want ErrStartRefused", err)
		}
	})
	t.Run("pre-check off: refused by the reducer or the database", func(t *testing.T) {
		st, e := setup(t)
		restore := native.DisablePrecheck()
		defer restore()
		_, err := native.Run(ctx, st, e.g, opts(true))
		if err == nil || errors.Is(err, native.ErrStartRefused) {
			t.Fatalf("run with the pre-check off: %v, want a refusal past native's checks", err)
		}
		if msg := err.Error(); !strings.Contains(msg, "anthropic-api differs from mission") || !strings.Contains(msg, "claude-code") {
			t.Fatalf("refusal %q does not name the path mismatch", msg)
		}
	})
}
