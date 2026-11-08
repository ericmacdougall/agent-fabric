package store

import (
	"context"
	"fmt"
	"os"
	"testing"
)

func TestApplyRolesRemovesPublicAndInheritedReviewReads(t *testing.T) {
	owner, ctx := openTest(t)
	r := testRoles(t, owner, ctx)
	runner := authorityRunnerLogin(t, owner, ctx, r)
	parent := fmt.Sprintf("fabric_reader_%d", os.Getpid())
	if _, err := owner.pool.Exec(ctx, "CREATE ROLE "+parent+" NOLOGIN"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		owner.pool.Exec(context.Background(), "DROP OWNED BY "+parent) //nolint:errcheck
		owner.pool.Exec(context.Background(), "DROP ROLE "+parent)     //nolint:errcheck
	})
	for _, q := range []string{
		"GRANT SELECT ON review_gate_requests, review_gate_results TO " + parent,
		"GRANT SELECT ON review_gate_requests, review_gate_results TO PUBLIC",
		"GRANT " + parent + " TO " + r.Runner(),
	} {
		if _, err := owner.pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := runner.CheckRunnerRole(ctx); err == nil {
		t.Fatal("runner admission accepted PUBLIC/inherited review reads")
	}
	if err := owner.ApplyRoles(ctx, r, "", authorityRunnerPassword); err != nil {
		t.Fatal(err)
	}
	if err := runner.CheckRunnerRole(ctx); err != nil {
		t.Fatalf("cleaned runner failed admission: %v", err)
	}
	for _, table := range []string{"review_gate_requests", "review_gate_results"} {
		var value string
		if err := runner.pool.QueryRow(ctx, "SELECT request_hash FROM "+table+" LIMIT 1").Scan(&value); sqlCode(err) != "42501" {
			t.Fatalf("runner SELECT %s: %v, want 42501", table, err)
		}
	}
}
