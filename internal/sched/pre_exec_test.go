package sched

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"fabric/internal/domain"
)

// goodqa#15966 AC-21: Launch.PreExec is the last check before the host
// execs the worker. A refusal starts nothing and abandons the attempt
// definite; a pass starts exactly one worker, and PreExec saw argv[0].
func TestPreExecRefusalStartsNothing(t *testing.T) {
	for _, refuse := range []bool{true, false} {
		id := domain.MissionID("m_px1")
		a := "r_px1_1"
		if !refuse {
			id, a = "m_px2", "r_px2_1"
		}
		st, ln, cfg := ownerSetup(t, id)
		var saw string
		cfg.Launch.PreExec = func(argv0 string) error {
			saw = argv0
			if refuse {
				return errors.New("pinned provider changed")
			}
			return nil
		}
		s := New(st, cfg, ln)
		if err := s.runOnce(context.Background(), id); err != nil {
			t.Fatal(err)
		}
		if saw == "" {
			t.Fatalf("refuse=%v: PreExec never ran", refuse)
		}
		n := atomic.LoadInt32(&ln.starts)
		got := attemptState(t, st, a)
		if refuse && (n != 0 || got != string(domain.AttemptFailedDefinite)) {
			t.Fatalf("refused pre-exec: %d starts, attempt %s; want 0, FAILED_DEFINITE", n, got)
		}
		if !refuse && n != 1 {
			t.Fatalf("passing pre-exec: %d starts, want 1", n)
		}
	}
}
