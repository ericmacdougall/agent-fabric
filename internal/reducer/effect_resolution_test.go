package reducer

import (
	"encoding/json"
	"errors"
	"testing"

	"fabric/internal/domain"
)

// Kimi effect-identity analysis, checked against fabric: a resolution must
// follow the path that can justify it, and it must name the submission it
// is about, so a late result for an earlier submission cannot settle a
// later one.

func submitted(t *testing.T) (*fleet, domain.MissionID, domain.EffectID) {
	t.Helper()
	f, id, a, fence := startedMission(t)
	ev := mustStep(t, f, domain.Command{Kind: domain.CmdSubmitEffect, Mission: id, Attempt: a, Fence: fence, EffectKind: "push", Request: json.RawMessage(`{"head":"sha_r"}`)})
	return f, id, ev[0].Effect
}

func TestUnknownCannotFailDefiniteWithoutReadback(t *testing.T) {
	f, id, eid := submitted(t)
	mustStep(t, f, domain.Command{Kind: domain.CmdEffectUnknown, Mission: id, Effect: eid, Submission: 1})
	if _, err := f.step(t, domain.Command{Kind: domain.CmdResolveEffect, Mission: id, Effect: eid, Submission: 1, Result: "FAILED_DEFINITE"}); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("UNKNOWN -> FAILED_DEFINITE: want refused, got %v", err)
	}
	if _, err := f.step(t, domain.Command{Kind: domain.CmdRetryEffect, Mission: id, Effect: eid}); !errors.Is(err, domain.ErrUnknownUnresolved) {
		t.Fatalf("retry: want ErrUnknownUnresolved, got %v", err)
	}
	if fx := f.missions[id].Effects[eid]; fx.State != domain.EffectUnknown || fx.Submissions != 1 {
		t.Fatalf("effect changed: %+v", fx)
	}
}

func TestNotAppliedNeedsUnknownFirst(t *testing.T) {
	f, id, eid := submitted(t)
	if _, err := f.step(t, domain.Command{Kind: domain.CmdResolveEffect, Mission: id, Effect: eid, Submission: 1, Result: "NOT_APPLIED"}); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("DISPATCHED -> NOT_APPLIED: want refused, got %v", err)
	}
	if fx := f.missions[id].Effects[eid]; fx.State != domain.EffectDispatched {
		t.Fatalf("effect changed: %+v", fx)
	}
}

func TestLateResultForEarlierSubmissionIsRefused(t *testing.T) {
	f, id, eid := submitted(t)
	mustStep(t, f, domain.Command{Kind: domain.CmdResolveEffect, Mission: id, Effect: eid, Submission: 1, Result: "FAILED_DEFINITE"})
	mustStep(t, f, domain.Command{Kind: domain.CmdRetryEffect, Mission: id, Effect: eid})
	for _, c := range []domain.Command{
		{Kind: domain.CmdResolveEffect, Mission: id, Effect: eid, Submission: 1, Result: "FAILED_DEFINITE"},
		{Kind: domain.CmdResolveEffect, Mission: id, Effect: eid, Result: "FAILED_DEFINITE"},
		{Kind: domain.CmdEffectUnknown, Mission: id, Effect: eid, Submission: 1},
	} {
		if _, err := f.step(t, c); !errors.Is(err, domain.ErrStaleFence) {
			t.Errorf("%s for submission %d while on 2: want ErrStaleFence, got %v", c.Kind, c.Submission, err)
		}
	}
	if fx := f.missions[id].Effects[eid]; fx.State != domain.EffectDispatched || fx.Submissions != 2 {
		t.Fatalf("submission 2 was settled by a late result: %+v", fx)
	}
	mustStep(t, f, domain.Command{Kind: domain.CmdResolveEffect, Mission: id, Effect: eid, Submission: 2, Result: "CONFIRMED"})
}
