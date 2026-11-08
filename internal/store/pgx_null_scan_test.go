package store

import (
	"strings"
	"testing"
)

// nullScanQuery returns SQL NULL. The NEG-4 failing control replaces it with
// a non-NULL value, which must make TestPgxNullIntoStringIsAnError fail.
const nullScanQuery = "SELECT NULL::text"

// goodqa#15299 NEG-4: pgx v5 refuses to scan SQL NULL into a non-pointer
// string, and fabric relies on that error instead of a silent "" for its
// nullable columns (attempts.worktree since 0038). The pgx 5.7.4 -> 5.9.2
// upgrade must keep the contract; a pointer still receives nil.
func TestPgxNullIntoStringIsAnError(t *testing.T) {
	st, ctx := openTest(t)
	var s string
	err := st.pool.QueryRow(ctx, nullScanQuery).Scan(&s)
	if err == nil {
		t.Fatalf("scanning NULL into a string returned no error (got %q)", s)
	}
	if !strings.Contains(err.Error(), "NULL") {
		t.Fatalf("error does not name the NULL: %v", err)
	}
	var p *string
	if err := st.pool.QueryRow(ctx, nullScanQuery).Scan(&p); err != nil || p != nil {
		t.Fatalf("NULL into *string: p=%v err=%v, want nil and no error", p, err)
	}
}
