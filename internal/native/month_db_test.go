package native_test

import (
	"context"
	"testing"

	"fabric/internal/agent"
	"fabric/internal/native"
	"fabric/internal/provider/anthropic/fakeapi"
	"fabric/internal/storetest"
)

// goodqa#15124 (red-team B5): the month spend cap is keyed on database
// time. Every month reservation of a run names the resource for the DB's
// UTC month and no other month resource exists. A host clock that differs
// from DB time (the controlled broken variant keys on a skewed host clock)
// fails this.
func TestMonthKeyIsDBTime(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, fakeapi.Step{Events: fakeapi.Text("finished", 50, 5)})
	st := storetest.Open(t)
	if _, err := native.Run(ctx, st, e.g, opts(true)); err != nil {
		t.Fatal(err)
	}
	m, err := st.DBMonth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := agent.SpendResource("month", m)
	rows, err := st.Pool().Query(ctx, `SELECT resource FROM reservations WHERE starts_with(resource, 'spend.month.')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			t.Fatal(err)
		}
		n++
		if r != want {
			t.Fatalf("month reservation on %s, want %s (DB month %s)", r, want, m)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("the run made no month reservation")
	}
}
