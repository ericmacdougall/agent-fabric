package native_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"fabric/internal/domain"
	"fabric/internal/native"
	"fabric/internal/provider/anthropic/fakeapi"
	"fabric/internal/storetest"
)

// goodqa#15235 S4: Run validates the grant itself, so a grant built in code
// (not loaded through Load) is refused before the key, a request or a write.
func TestRunValidatesGrantBeforeKeyOrWrite(t *testing.T) {
	e := newEnv(t, fakeapi.Step{Events: fakeapi.Text("finished", 10, 1)})
	e.g.Caps.Month = 25_000_000 // over the fixed USD 20 ceiling
	st := storetest.Open(t)
	_, err := native.Run(context.Background(), st, e.g, opts(true))
	if err == nil || !strings.Contains(err.Error(), "ceilings") {
		t.Fatalf("over-ceiling grant was not refused by Validate: %v", err)
	}
	if _, statErr := os.Stat(e.marker); statErr == nil {
		t.Fatal("the key was read for an invalid grant")
	}
	if n := len(e.srv.Requests()); n != 0 {
		t.Fatalf("%d requests sent for an invalid grant", n)
	}
	var n int
	if err := st.Pool().QueryRow(context.Background(), `SELECT count(*) FROM attempts WHERE id = $1`, e.g.Attempt).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("an invalid grant created %d attempt rows", n)
	}
}

// goodqa#15235 S4: a second runner that also planned a new start replays the
// keyed start command and would get the same fence, running the attempt twice.
// The replay is refused instead.
func TestStartNativeRefusesReplayedStart(t *testing.T) {
	e := newEnv(t)
	st := storetest.Open(t)
	ctx := context.Background()
	lockPath, err := native.LockPath(domain.AttemptID(e.g.Attempt))
	if err != nil {
		t.Fatal(err)
	}
	fence, err := native.StartNativeForTest(ctx, st, e.g, true, lockPath)
	if err != nil || fence <= 0 {
		t.Fatalf("first start: fence %d err %v", fence, err)
	}
	fence2, err := native.StartNativeForTest(ctx, st, e.g, true, lockPath)
	if !errors.Is(err, domain.ErrInvalidTransition) || !strings.Contains(err.Error(), "concurrently") {
		t.Fatalf("replayed start: fence %d err %v, want ErrInvalidTransition (created concurrently)", fence2, err)
	}
}
