package sched

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"fabric/internal/credential"
	"fabric/internal/domain"
)

// goodqa#15966 seam 5, AC-17: a token the admission rule refuses starts
// nothing: no attempt, no event, 0 processes, and no binding record. Once
// the token is admissible again the next attempt starts.
func TestM1AdmissionRefusedBeforeStart(t *testing.T) {
	ctx := context.Background()
	const m = domain.MissionID("m_adm")
	st, ln, cfg, bindings := tokenRig(t, m)
	// The refusing policy: claude-oauth is gone, so the current version is
	// no longer a hard link that passes every check.
	if err := os.Remove(filepath.Join(cfg.Credential.Policy.Dir, credential.CurrentName)); err != nil {
		t.Fatal(err)
	}
	if cfg.Credential.Policy.CheckAdmission() == nil {
		t.Fatal("fixture: the policy still admits")
	}
	countEv := func() int {
		var n int
		st.Pool().QueryRow(ctx, `SELECT count(*) FROM events WHERE mission_id = $1`, string(m)).Scan(&n) //nolint:errcheck
		return n
	}
	before := countEv()
	s := New(st, cfg, ln)
	err := s.runOnce(ctx, m)
	if err == nil || !strings.Contains(err.Error(), classAdmission) {
		t.Fatalf("runOnce: %v, want a %s refusal", err, classAdmission)
	}
	var attempts int
	st.Pool().QueryRow(ctx, `SELECT count(*) FROM attempts WHERE mission_id = $1`, string(m)).Scan(&attempts) //nolint:errcheck
	if after := countEv(); attempts != 0 || after != before || atomic.LoadInt32(&ln.starts) != 0 {
		t.Fatalf("attempts %d, events %d -> %d, processes %d; want no attempt, no event, 0 processes", attempts, before, after, atomic.LoadInt32(&ln.starts))
	}
	if left, _ := os.ReadDir(bindings); len(left) != 0 {
		t.Fatalf("binding records written: %v", left)
	}
	dir := cfg.Credential.Policy.Dir
	if err := os.Link(filepath.Join(dir, credential.VersionName(0)), filepath.Join(dir, credential.CurrentName)); err != nil {
		t.Fatal(err)
	}
	if err := s.runOnce(ctx, m); err != nil {
		t.Fatal(err)
	}
	waitStarts(t, ln, 1)
}
