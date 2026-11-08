package store

import "testing"

// Migration 0031 repins the reservation backstops to pg_catalog, pg_temp. This
// reads a database migrated from scratch so another test that re-applies an
// older migration (0029 rewrites any path it does not recognise) cannot hide a
// function left on a shadowable path.
func TestMigration0031PinsBackstopsToCatalogAndTempOnly(t *testing.T) {
	st, ctx, _ := freshStore(t, "")
	rows, err := st.pool.Query(ctx, `SELECT p.proname, COALESCE((SELECT substring(c FROM 'search_path=(.*)')
			FROM unnest(p.proconfig) c WHERE c LIKE 'search_path=%'), '')
		FROM pg_proc p WHERE p.pronamespace = 'public'::regnamespace AND p.proname = ANY($1)`,
		[]string{"fabric_reject_mutation", "fabric_reservation_update", "fabric_capacity_agrees",
			"fabric_reservation_history", "fabric_reservation_child_agrees", "fabric_reservation_insert", "fabric_reject_truncate"})
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var name, path string
		if err := rows.Scan(&name, &path); err != nil {
			t.Fatal(err)
		}
		n++
		if path != "pg_catalog, pg_temp" {
			t.Errorf("%s search_path = %q, want \"pg_catalog, pg_temp\"", name, path)
		}
	}
	if n != 7 {
		t.Fatalf("found %d of the 7 backstop functions", n)
	}
}
