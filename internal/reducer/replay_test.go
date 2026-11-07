package reducer

import (
	"errors"
	"testing"

	"fabric/internal/domain"
)

// Kimi compatibility analysis, checked against fabric: Replay folded through
// the permissive Apply, so an event kind this binary cannot interpret was
// skipped and the events after it were still applied. Replay is the
// authority boundary and now stops at the first unsupported kind.

func TestKnownEventKindRegistryIsComplete(t *testing.T) {
	for _, k := range []domain.EventKind{
		domain.EvMissionAdmitted, domain.EvIntentRevised, domain.EvAttemptStarted, domain.EvAttemptAcked,
		domain.EvEvidenceRecorded, domain.EvAttemptFinished, domain.EvAttemptUnknown, domain.EvAttemptReconciled,
		domain.EvCandidateSelected, domain.EvCandidateAccepted, domain.EvDelivered, domain.EvWakeRecorded,
		domain.EvWakeAcked, domain.EvMissionResumed, domain.EvEffectSubmitted, domain.EvEffectUnknown,
		domain.EvEffectResubmitted, domain.EvEffectResolved, domain.EvMissionReleased, domain.EvAcceptanceRevoked,
		domain.EvClaimRefused, domain.EvDispatchDeferred, domain.EvMissionParked, domain.EvMissionUnpaused,
	} {
		if !domain.KnownEventKind(k) {
			t.Errorf("%s is declared but not known", k)
		}
	}
	for _, k := range []domain.EventKind{"", "FUTURE_PUBLICATION", "WAKE_RECORDED_V2", "wake_recorded"} {
		if domain.KnownEventKind(k) {
			t.Errorf("%q is known", k)
		}
	}
}

func TestReplayStopsAtFirstUnknownEvent(t *testing.T) {
	log := []domain.Event{
		{Kind: domain.EvMissionAdmitted, Mission: "m_r", Seq: 1},
		{Kind: "FUTURE_PUBLICATION", Mission: "m_r", Seq: 2},
		{Kind: domain.EvWakeRecorded, Mission: "m_r", Seq: 3, Token: "after-future"},
	}
	m, err := Replay("m_r", log)
	if !errors.Is(err, domain.ErrUnsupportedEvent) {
		t.Fatalf("want ErrUnsupportedEvent, got %v", err)
	}
	if m.Seq != 1 || m.Wakes["after-future"] || m.Execution != domain.ExecAdmitted {
		t.Fatalf("replay went past the unknown event: seq %d, wakes %v, execution %s", m.Seq, m.Wakes, m.Execution)
	}
	if _, err := Replay("m_r", log[1:]); !errors.Is(err, domain.ErrUnsupportedEvent) {
		t.Fatalf("unknown first event: %v", err)
	}
	if _, err := Replay("m_r", log[:1]); err != nil {
		t.Fatalf("known log: %v", err)
	}
}
