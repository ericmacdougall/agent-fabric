package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fabric/internal/credential"
	"fabric/internal/domain"
	"fabric/internal/records"
	"fabric/internal/storetest"
)

// goodqa#15966 F3: `fab status` shows each operator-held mission with its
// hold class, and the bindings_lost alert with the runbook anchor.
func TestStatusShowsOperatorHoldsAndBindingsLost(t *testing.T) {
	const m = domain.MissionID("m_st")
	ctx, st := releaseFixture(t, m)
	t.Setenv("FABRIC_DSN", storetest.DSN)
	if err := st.OperatorHold(ctx, m); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "records")
	for _, r := range []records.Record{
		{Mission: m, Attempt: "r_st_1", Kind: records.MissionHold, Class: credential.ClassMaterialUnavailable},
		{Mission: m, Attempt: "r_st_1", Kind: records.BindingsLost, Attempts: 3, Missions: 2},
	} {
		if err := records.Append(dir, r); err != nil {
			t.Fatal(err)
		}
	}
	run := func(args ...string) string {
		t.Helper()
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		saved := os.Stdout
		os.Stdout = w
		done := make(chan string)
		go func() { b, _ := io.ReadAll(r); done <- string(b) }()
		serr := statusMain(args)
		os.Stdout = saved
		w.Close()
		out := <-done
		r.Close()
		if serr != nil {
			t.Fatal(serr)
		}
		return out
	}
	out := run("--records-dir", dir)
	for _, want := range []string{
		"operator holds: 1",
		"  m_st  class: credential_material_unavailable  attempt: r_st_1",
		"3 M1 attempts, 2 missions held; see docs/m1-runbook.md#bindings-lost",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("fab status lacks %q:\n%s", want, out)
		}
	}
	if out := run(); !strings.Contains(out, "operator holds: 1\n  m_st  class: not shown") {
		t.Fatalf("fab status without a records directory:\n%s", out)
	}
}
