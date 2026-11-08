package native_test

import (
	"context"
	"testing"

	"fabric/internal/native"
	"fabric/internal/store"
	"fabric/internal/storetest"
)

// openNativeStore adds review-table isolation to the pre-review shared fixture.
func openNativeStore(t *testing.T) *store.Store {
	t.Helper()
	st := storetest.Open(t)
	tx, err := st.Pool().Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"SET LOCAL fabric.allow_truncate = 'on'",
		"TRUNCATE review_gate_approvals, review_gate_audit, review_gate_requests, review_gate_circuits RESTART IDENTITY CASCADE",
		"UPDATE review_gate_audit_head SET seq = 0, last_hash = '' WHERE id = 1",
	} {
		if _, err := tx.Exec(context.Background(), q); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	return st
}

// gateOpts is opts with the review gate switched on and the app-role review
// DSN supplied; every other native test runs flag-off through opts.
func gateOpts(contained bool) native.Options {
	o := opts(contained)
	o.ReviewGate = true
	o.ReviewDSN = storetest.DSN
	return o
}
