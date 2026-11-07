package native_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"fabric/internal/agent"
	"fabric/internal/native"
	"fabric/internal/provider/anthropic/fakeapi"
	"fabric/internal/store"
	"fabric/internal/storetest"
)

// monthResources lists the distinct month spend resources of the store.
func monthResources(t *testing.T, st *store.Store) []string {
	t.Helper()
	rows, err := st.Pool().Query(context.Background(), `SELECT DISTINCT resource FROM reservations WHERE starts_with(resource, 'spend.month.')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// goodqa#15124 (red-team B5; inspection finding 1): Run keys the month cap
// on its month source and never on the host clock. The source is replaced
// with a month that no host clock gives (the host month shifted by 40
// months), so code that reads time.Now, the pre-fix line included, keys a
// different resource and fails. The default source is the DB month (the
// second case, against Store.DBMonth).
func TestMonthKeyIsDBTime(t *testing.T) {
	ctx := context.Background()
	t.Run("the month source decides, not the host clock", func(t *testing.T) {
		skewed := time.Now().UTC().AddDate(0, 40, 0).Format("200601")
		defer native.SetMonthSource(func(context.Context, *store.Store) (string, error) { return skewed, nil })()
		e := newEnv(t, fakeapi.Step{Events: fakeapi.Text("finished", 50, 5)})
		st := storetest.Open(t)
		if _, err := native.Run(ctx, st, e.g, opts(true)); err != nil {
			t.Fatal(err)
		}
		want := agent.SpendResource("month", skewed)
		if got := monthResources(t, st); len(got) != 1 || got[0] != want {
			t.Fatalf("month reservations on %v, want only %s (month %s from the source)", got, want, skewed)
		}
	})
	t.Run("the default source reads the database", func(t *testing.T) {
		// Delta inspection 5ec02ad: a default that never touches the DB (an
		// unskewed host clock) agrees with the DB month most of the time;
		// against a closed store it returns no error and is caught here.
		st := storetest.Open(t)
		st.Close()
		if m, err := native.DefaultMonthSource(ctx, st); err == nil {
			t.Fatalf("default month source returned %q from a closed store; it does not read the database", m)
		}
	})
	t.Run("a month source error stops Run before the attempt starts", func(t *testing.T) {
		defer native.SetMonthSource(func(context.Context, *store.Store) (string, error) { return "", errors.New("db down") })()
		e := newEnv(t, fakeapi.Step{Events: fakeapi.Text("never", 50, 5)})
		st := storetest.Open(t)
		_, err := native.Run(ctx, st, e.g, opts(true))
		if err == nil || !strings.Contains(err.Error(), "month key") {
			t.Fatalf("run with a failing month source: %v", err)
		}
		if n := count(t, st, "SELECT count(*) FROM attempts"); n != 0 || len(e.srv.Requests()) != 0 {
			t.Fatalf("%d attempts, %d requests after a month-source failure; want 0, 0", n, len(e.srv.Requests()))
		}
		if n := count(t, st, "SELECT count(*) FROM reservations"); n != 0 {
			t.Fatalf("%d reservations after a month-source failure", n)
		}
	})
	t.Run("the default source is the DB month", func(t *testing.T) {
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
		if got := monthResources(t, st); len(got) != 1 || got[0] != want {
			t.Fatalf("month reservations on %v, want only %s (DB month %s)", got, want, m)
		}
	})
}
