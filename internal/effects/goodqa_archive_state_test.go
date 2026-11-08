package effects

import (
	"context"
	"testing"
)

// goodqa#15260 F1: a comment that was posted, lost its reply and was then
// archived by retention must still read back as COMMITTED. GoodQA hides
// archived comments unless the readback asks for archiveState=all, so a
// default listing would read NOT_APPLIED and let a retry post twice.
func TestGoodQALostReplyThenArchivedIsCommitted(t *testing.T) {
	ctx, _, b, _ := setup(t)
	f, g := fakeServer(t)
	f.set(func() { f.loseAfter = true })
	e, err := b.Submit(ctx, g, gqSpec("gq-archived", "checkpoint: archived later"))
	if err != nil {
		t.Fatal(err)
	}
	if e, _ = b.Dispatch(ctx, g, e.ID); e.State != Unknown || f.count() != 1 {
		t.Fatalf("lost reply: %s comments=%d, want UNKNOWN with 1 comment", e.State, f.count())
	}
	f.set(func() {
		f.loseAfter = false
		f.comments[0]["archivedAt"] = "2026-11-08T00:00:00Z"
	})
	obs, detail, err := g.Reconcile(ctx, e)
	if err != nil || obs != ObsCommitted {
		t.Fatalf("archived after a lost reply: %s %q %v, want COMMITTED", obs, detail, err)
	}
}

// Every readback page asks for every archive state, including continuation
// pages (the server requires the same archiveState with a cursor).
func TestGoodQAReadbackAsksForAllArchiveStates(t *testing.T) {
	f, g := fakeServer(t)
	e := Effect{IdemKey: "absent-key", Request: gqSpec("absent-key", "regression").Request}
	obs, detail, err := g.Reconcile(context.Background(), e)
	if err != nil || obs != ObsNotApplied {
		t.Fatalf("empty listing: %s %q %v, want NOT_APPLIED", obs, detail, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.archiveStates) == 0 {
		t.Fatal("no readback call was made")
	}
	for i, s := range f.archiveStates {
		if s != "all" {
			t.Fatalf("readback call %d sent archiveState %v, want all", i, s)
		}
	}
}

// The fake must hide archived rows by default, or the tests above are vacuous.
func TestFakeGoodQAHidesArchivedByDefault(t *testing.T) {
	rows := []map[string]any{{"id": "a", "body": "x"}, {"id": "b", "body": "y", "archivedAt": "t"}}
	for state, want := range map[any]int{nil: 1, "active": 1, "archived": 1, "all": 2} {
		got, ok := filterArchiveState(rows, state)
		if !ok || len(got) != want {
			t.Fatalf("archiveState %v: %d rows ok=%v, want %d", state, len(got), ok, want)
		}
	}
	if _, ok := filterArchiveState(rows, "bogus"); ok {
		t.Fatal("unknown archiveState accepted")
	}
}
