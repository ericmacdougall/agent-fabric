package effects

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// goodqa#15260: a readback that runs out of page budget can never settle on
// a later pass (the thread only grows), and fab effect resolve accepts only
// QUARANTINED. So an over-budget effect must quarantine, with the budget
// named, and the operator exit must then work.
func TestGoodQAOverBudgetReadbackQuarantinesAndResolves(t *testing.T) {
	ctx, _, b, _ := setup(t)
	f, g := fakeServer(t)
	f.set(func() { f.loseAfter = true })
	e, err := b.Submit(ctx, g, gqSpec("gq-budget", "checkpoint: long thread"))
	if err != nil {
		t.Fatal(err)
	}
	if e, _ = b.Dispatch(ctx, g, e.ID); e.State != Unknown {
		t.Fatalf("lost reply: %s, want UNKNOWN", e.State)
	}
	// Every page is valid, empty and points at a fresh cursor: more history
	// than the budget allows, with no malformed page to blame.
	var mu sync.Mutex
	pages := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		pages++
		n := pages
		mu.Unlock()
		writeMCPText(t, w, fmt.Sprintf(`{"comments":[],"hasMore":true,"nextCursor":"c%d"}`, n), false)
	}))
	defer srv.Close()
	long := goodQAForEndpoint(t, srv.URL)
	long.MaxPages = 2
	got, err := b.Reconcile(ctx, long, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != Quarantined || !strings.Contains(got.LastError, "comment history exceeded the page budget") {
		t.Fatalf("over budget: %s %q, want QUARANTINED naming the page budget", got.State, got.LastError)
	}
	if pages != 2 {
		t.Fatalf("readback read %d pages, want exactly the budget of 2", pages)
	}
	res, err := b.ResolveQuarantined(ctx, e.ID, ResolvedApplied, "checked the ticket by hand", "ticket:goodqa#14913", "op:test")
	if err != nil || res.State != ResolvedApplied {
		t.Fatalf("resolve after over-budget quarantine: %s %v, want RESOLVED_APPLIED", res.State, err)
	}
}

// A transient readback failure still stays UNKNOWN for a later pass.
func TestGoodQAMalformedReadbackStaysUnknownInBroker(t *testing.T) {
	ctx, _, b, _ := setup(t)
	f, g := fakeServer(t)
	f.set(func() { f.loseAfter = true })
	e, err := b.Submit(ctx, g, gqSpec("gq-transient", "checkpoint: transient"))
	if err != nil {
		t.Fatal(err)
	}
	if e, _ = b.Dispatch(ctx, g, e.ID); e.State != Unknown {
		t.Fatalf("lost reply: %s, want UNKNOWN", e.State)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeMCPText(t, w, `{"comments":[]}`, false)
	}))
	defer srv.Close()
	got, err := b.Reconcile(ctx, goodQAForEndpoint(t, srv.URL), e.ID)
	if err != nil || got.State != Unknown {
		t.Fatalf("malformed page: %s %v, want UNKNOWN", got.State, err)
	}
}
