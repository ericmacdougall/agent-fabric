package reducer

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"testing"

	"fabric/internal/domain"
)

// fleet is the property-test harness: several missions sharing one cap, so
// the fleet-level slot invariant (R1) is exercised the way the store does it.
type fleet struct {
	cap      int
	missions map[domain.MissionID]domain.Mission
	logs     map[domain.MissionID][]domain.Event
	refusals map[string]int
	emitted  map[domain.EventKind]int
}

func newFleet(capacity int, n int) *fleet {
	f := &fleet{cap: capacity, missions: map[domain.MissionID]domain.Mission{}, logs: map[domain.MissionID][]domain.Event{},
		refusals: map[string]int{}, emitted: map[domain.EventKind]int{}}
	for i := 0; i < n; i++ {
		id := domain.MissionID(fmt.Sprintf("m_%d", i))
		f.missions[id] = domain.NewMission(id)
	}
	return f
}

func (f *fleet) env(id domain.MissionID) Env {
	used := 0
	for mid, m := range f.missions {
		if mid != id && m.Execution.HoldsSlot() {
			used++
		}
	}
	return Env{Cap: f.cap, SlotsUsed: used}
}

// step runs one command through Decide/Apply/Check exactly as the store does.
func (f *fleet) step(t *testing.T, cmd domain.Command) (events []domain.Event, err error) {
	t.Helper()
	before := f.missions[cmd.Mission]
	env := f.env(cmd.Mission)
	events, err = Decide(before, cmd, env)
	after := before
	for i := range events {
		events[i].Seq = after.Seq + 1
		after = Apply(after, events[i])
		f.emitted[events[i].Kind]++
	}
	if err != nil {
		f.refusals[rootName(err)]++
	}
	if v := Check(before, after, cmd, events, err, env); v != nil {
		t.Fatalf("invariant violated: %v\ncmd=%+v\nbefore=%s\nafter=%s", v, cmd, Digest(before), Digest(after))
	}
	f.missions[cmd.Mission] = after
	f.logs[cmd.Mission] = append(f.logs[cmd.Mission], events...)
	// Fleet-level cap: never more slot holders than the cap.
	held := 0
	for _, m := range f.missions {
		if m.Execution.HoldsSlot() {
			held++
		}
	}
	if held > f.cap {
		t.Fatalf("%s: %d missions hold slots, cap %d (cmd=%+v)", InvCapacity, held, f.cap, cmd)
	}
	return events, err
}

func rootName(err error) string {
	for _, e := range []error{domain.ErrStaleFence, domain.ErrOverCapacity, domain.ErrUnknownUnresolved, domain.ErrEvidenceStale,
		domain.ErrGenerationStale, domain.ErrDuplicate, domain.ErrNotFound, domain.ErrAttemptLive, domain.ErrInvalidTransition} {
		if errors.Is(err, e) {
			return e.Error()
		}
	}
	return "other"
}

// randomCommand picks a mostly-legal command with deliberate stale, duplicate
// and over-cap attempts mixed in, so every refusal path is hit.
func randomCommand(r *rand.Rand, f *fleet, id domain.MissionID, step int) domain.Command {
	m := f.missions[id]
	c := domain.Command{Mission: id}
	var attemptIDs []domain.AttemptID
	for aid := range m.Attempts {
		attemptIDs = append(attemptIDs, aid)
	}
	sortAttempts(attemptIDs)
	pickAttempt := func() domain.AttemptID {
		if len(attemptIDs) == 0 {
			return "r_none"
		}
		return attemptIDs[r.Intn(len(attemptIDs))]
	}
	fenceFor := func(aid domain.AttemptID) int64 {
		a, ok := m.Attempts[aid]
		if !ok {
			return 1
		}
		if r.Intn(5) == 0 { // one in five messages carries a stale or forged fence
			return a.Fence - 1 + int64(r.Intn(3))*2 // -1 or +1 or +3
		}
		return a.Fence
	}
	var effectIDs []domain.EffectID
	for eid := range m.Effects {
		effectIDs = append(effectIDs, eid)
	}
	sortEffects(effectIDs)
	// Weighted mix: acceptance and delivery need several legal steps in a row,
	// so intent revisions and candidate selections are rare.
	pick := r.Intn(100)
	kind := 0
	for i, w := range cmdWeights {
		if pick < w {
			kind = i
			break
		}
		pick -= w
	}
	switch kind {
	case 0:
		c.Kind = domain.CmdAdmitMission
	case 1:
		c.Kind = domain.CmdStartAttempt
		c.Attempt = domain.AttemptID(fmt.Sprintf("r_%s_%d", id, step))
		if r.Intn(6) == 0 && len(attemptIDs) > 0 {
			c.Attempt = pickAttempt() // duplicate id
		}
		c.Host = "h_t2"
		c.Role = []string{"writer", "reviewer"}[r.Intn(2)]
	case 2:
		c.Kind = domain.CmdAckStart
		c.Attempt = pickAttempt()
		c.Fence = fenceFor(c.Attempt)
	case 3:
		c.Kind = domain.CmdRecordEvidence
		c.Attempt = pickAttempt()
		c.Fence = fenceFor(c.Attempt)
		c.Revision = m.Version.IntentRevision - int64(r.Intn(4)/3)
		c.Generation = m.Version.CandidateGeneration - int64(r.Intn(4)/3)
		c.Head = m.Head
		if r.Intn(4) == 0 {
			c.Head = "sha_other"
		}
		c.Receipt = fmt.Sprintf("rcpt_%d", step)
	case 4:
		c.Kind = domain.CmdFinishAttempt
		c.Attempt = pickAttempt()
		c.Fence = fenceFor(c.Attempt)
		c.Outcome = []string{"checkpoint", "wait", "ready_for_review", "blocked", "abandoned", "bogus"}[r.Intn(6)]
		c.WaitKey = fmt.Sprintf("job_%d", r.Intn(3))
	case 5:
		c.Kind = domain.CmdExpireLease
		c.Attempt = pickAttempt()
		c.Fence = fenceFor(c.Attempt)
	case 6:
		c.Kind = domain.CmdReconcileAttempt
		c.Attempt = pickAttempt()
		c.Fence = fenceFor(c.Attempt)
		c.Result = []string{"VERIFIED", "FAILED_DEFINITE", "QUARANTINED", "VERIFIED", "VERIFIED"}[r.Intn(5)]
	case 7:
		c.Kind = domain.CmdSelectCandidate
		c.Head = fmt.Sprintf("sha_%d", r.Intn(4))
		c.Attempt = pickAttempt()
	case 8:
		c.Kind = domain.CmdAccept
		c.Attempt = pickAttempt()
	case 9:
		c.Kind = domain.CmdDeliver
		c.Attempt = pickAttempt()
		c.Generation = m.Version.CandidateGeneration - int64(r.Intn(2))
	case 10:
		c.Kind = domain.CmdWake
		c.Token = fmt.Sprintf("job_%d", r.Intn(3))
	case 11:
		c.Kind = domain.CmdAckWake
		c.Token = fmt.Sprintf("job_%d", r.Intn(3))
	case 12:
		c.Kind = domain.CmdSubmitEffect
		c.Attempt = pickAttempt()
		c.Fence = fenceFor(c.Attempt)
		c.Effect = domain.EffectID(fmt.Sprintf("e_%s_%d", id, r.Intn(4)))
	case 13:
		c.Kind = domain.CmdEffectUnknown
		if len(effectIDs) > 0 {
			c.Effect = effectIDs[r.Intn(len(effectIDs))]
		}
	case 14:
		c.Kind = domain.CmdRetryEffect
		if len(effectIDs) > 0 {
			c.Effect = effectIDs[r.Intn(len(effectIDs))]
		}
	case 15:
		c.Kind = domain.CmdResolveEffect
		if len(effectIDs) > 0 {
			c.Effect = effectIDs[r.Intn(len(effectIDs))]
		}
		c.Result = []string{"CONFIRMED", "FAILED_DEFINITE", "NOT_APPLIED", "QUARANTINED"}[r.Intn(4)]
	case 16:
		c.Kind = domain.CmdRelease
	case 17:
		c.Kind = domain.CmdReviseIntent
		c.Reason = "user input"
	}
	return c
}

// cmdWeights is indexed by the case numbers below and sums to 100.
var cmdWeights = []int{8, 10, 8, 9, 10, 4, 8, 3, 8, 8, 5, 2, 4, 3, 3, 3, 2, 2}

func sortAttempts(a []domain.AttemptID) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}
func sortEffects(a []domain.EffectID) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

// TestTraces is the N1-style property run: seeded random traces over a fleet,
// every transition checked, every refusal path required to have fired at least
// once (no vacuous pass), and the event log replayed to the same digest.
func TestTraces(t *testing.T) {
	traces, steps := 500, 300
	base := int64(20261106)
	if s := os.Getenv("FABRIC_SEED"); s != "" {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			t.Fatalf("FABRIC_SEED: %v", err)
		}
		base = v
	}
	total := map[string]int{}
	emitted := map[domain.EventKind]int{}
	var checks int
	for i := 0; i < traces; i++ {
		seed := base + int64(i)*1_000_003
		r := rand.New(rand.NewSource(seed))
		f := newFleet(1+r.Intn(3), 2+r.Intn(3))
		ids := make([]domain.MissionID, 0, len(f.missions))
		for id := range f.missions {
			ids = append(ids, id)
		}
		sortMissions(ids)
		for s := 0; s < steps; s++ {
			id := ids[r.Intn(len(ids))]
			cmd := randomCommand(r, f, id, s)
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						t.Fatalf("seed %d step %d: panic %v on %+v", seed, s, rec, cmd)
					}
				}()
				f.step(t, cmd)
			}()
			checks++
		}
		for id, m := range f.missions {
			if got := Digest(Replay(id, f.logs[id])); got != Digest(m) {
				t.Fatalf("seed %d: replay of %s differs from live state", seed, id)
			}
		}
		for k, v := range f.refusals {
			total[k] += v
		}
		for k, v := range f.emitted {
			emitted[k] += v
		}
	}
	// Coverage gates: each safety path must have been exercised.
	need := []string{domain.ErrStaleFence.Error(), domain.ErrOverCapacity.Error(), domain.ErrUnknownUnresolved.Error(),
		domain.ErrEvidenceStale.Error(), domain.ErrGenerationStale.Error(), domain.ErrDuplicate.Error(), domain.ErrAttemptLive.Error()}
	for _, n := range need {
		if total[n] == 0 {
			t.Errorf("refusal %q never exercised (seed base %d)", n, base)
		}
	}
	for _, k := range []domain.EventKind{domain.EvCandidateAccepted, domain.EvDelivered, domain.EvMissionResumed, domain.EvEffectResubmitted,
		domain.EvAttemptUnknown, domain.EvAttemptReconciled, domain.EvAcceptanceRevoked} {
		if emitted[k] == 0 {
			t.Errorf("event %s never emitted (seed base %d)", k, base)
		}
	}
	t.Logf("traces=%d steps=%d checks=%d seed_base=%d refusals=%v", traces, steps, checks, base, total)
	t.Logf("emitted=%v", emitted)
}

func sortMissions(a []domain.MissionID) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

// --- Targeted tests, one per invariant. Each has a mutant in
// tools/mutants/mutants.tsv that must make it fail.

func mustStep(t *testing.T, f *fleet, cmd domain.Command) []domain.Event {
	t.Helper()
	ev, err := f.step(t, cmd)
	if err != nil {
		t.Fatalf("%s refused: %v", cmd.Kind, err)
	}
	return ev
}

func startedMission(t *testing.T) (*fleet, domain.MissionID, domain.AttemptID, int64) {
	t.Helper()
	f := newFleet(4, 1)
	id := domain.MissionID("m_0")
	mustStep(t, f, domain.Command{Kind: domain.CmdAdmitMission, Mission: id})
	mustStep(t, f, domain.Command{Kind: domain.CmdSelectCandidate, Mission: id, Head: "sha_a"})
	ev := mustStep(t, f, domain.Command{Kind: domain.CmdStartAttempt, Mission: id, Attempt: "r_1", Host: "h_t2", Role: "writer"})
	fence := ev[0].Fence
	mustStep(t, f, domain.Command{Kind: domain.CmdAckStart, Mission: id, Attempt: "r_1", Fence: fence})
	return f, id, "r_1", fence
}

func TestStaleFenceRefused(t *testing.T) {
	f, id, a, fence := startedMission(t)
	before := f.missions[id]
	_, err := f.step(t, domain.Command{Kind: domain.CmdFinishAttempt, Mission: id, Attempt: a, Fence: fence - 1, Outcome: "checkpoint"})
	if !errors.Is(err, domain.ErrStaleFence) {
		t.Fatalf("want ErrStaleFence, got %v", err)
	}
	if Digest(before) != Digest(f.missions[id]) {
		t.Fatal("stale fence changed state")
	}
	_, err = f.step(t, domain.Command{Kind: domain.CmdFinishAttempt, Mission: id, Attempt: a, Fence: fence + 1, Outcome: "checkpoint"})
	if !errors.Is(err, domain.ErrStaleFence) {
		t.Fatalf("forged future fence: want ErrStaleFence, got %v", err)
	}
}

func TestDuplicateWakeIdempotent(t *testing.T) {
	f := newFleet(4, 1)
	id := domain.MissionID("m_0")
	ev := mustStep(t, f, domain.Command{Kind: domain.CmdWake, Mission: id, Token: "job_1"})
	if len(ev) != 1 {
		t.Fatalf("first wake: want 1 event, got %d", len(ev))
	}
	ev = mustStep(t, f, domain.Command{Kind: domain.CmdWake, Mission: id, Token: "job_1"})
	if len(ev) != 0 {
		t.Fatalf("duplicate wake: want 0 events, got %d", len(ev))
	}
}

func TestWakeResumesWaitingMission(t *testing.T) {
	f, id, a, fence := startedMission(t)
	mustStep(t, f, domain.Command{Kind: domain.CmdFinishAttempt, Mission: id, Attempt: a, Fence: fence, Outcome: "wait", WaitKey: "job_7"})
	if f.missions[id].Execution != domain.ExecWaiting {
		t.Fatalf("want WAITING, got %s", f.missions[id].Execution)
	}
	mustStep(t, f, domain.Command{Kind: domain.CmdWake, Mission: id, Token: "job_other"})
	if f.missions[id].Execution != domain.ExecWaiting {
		t.Fatal("unrelated wake resumed the mission")
	}
	mustStep(t, f, domain.Command{Kind: domain.CmdWake, Mission: id, Token: "job_7"})
	if f.missions[id].Execution != domain.ExecAdmitted {
		t.Fatalf("want ADMITTED after wake, got %s", f.missions[id].Execution)
	}
}

func TestCapacityIsRefused(t *testing.T) {
	f := newFleet(2, 3)
	mustStep(t, f, domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_0"})
	mustStep(t, f, domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_1"})
	_, err := f.step(t, domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_2"})
	if !errors.Is(err, domain.ErrOverCapacity) {
		t.Fatalf("third admission with cap 2: want ErrOverCapacity, got %v", err)
	}
	mustStep(t, f, domain.Command{Kind: domain.CmdRelease, Mission: "m_0"})
	mustStep(t, f, domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_2"})
}

func TestPerMissionLiveAttemptsBounded(t *testing.T) {
	f, id, _, _ := startedMission(t)
	mustStep(t, f, domain.Command{Kind: domain.CmdStartAttempt, Mission: id, Attempt: "r_2", Host: "h_t2", Role: "reviewer"})
	_, err := f.step(t, domain.Command{Kind: domain.CmdStartAttempt, Mission: id, Attempt: "r_3", Host: "h_t2", Role: "writer"})
	if !errors.Is(err, domain.ErrAttemptLive) {
		t.Fatalf("third live attempt: want ErrAttemptLive, got %v", err)
	}
}

func TestUnknownRetryBlocked(t *testing.T) {
	f, id, a, fence := startedMission(t)
	mustStep(t, f, domain.Command{Kind: domain.CmdSubmitEffect, Mission: id, Attempt: a, Fence: fence, Effect: "e_push"})
	mustStep(t, f, domain.Command{Kind: domain.CmdEffectUnknown, Mission: id, Effect: "e_push", Reason: "timeout"})
	_, err := f.step(t, domain.Command{Kind: domain.CmdRetryEffect, Mission: id, Effect: "e_push"})
	if !errors.Is(err, domain.ErrUnknownUnresolved) {
		t.Fatalf("retry during UNKNOWN: want ErrUnknownUnresolved, got %v", err)
	}
	if got := f.missions[id].Effects["e_push"].Submissions; got != 1 {
		t.Fatalf("submissions changed to %d", got)
	}
	mustStep(t, f, domain.Command{Kind: domain.CmdResolveEffect, Mission: id, Effect: "e_push", Result: "NOT_APPLIED"})
	mustStep(t, f, domain.Command{Kind: domain.CmdRetryEffect, Mission: id, Effect: "e_push"})
	if got := f.missions[id].Effects["e_push"].Submissions; got != 2 {
		t.Fatalf("after NOT_APPLIED, retry should be the 2nd submission, got %d", got)
	}
}

func TestAcceptNeedsCurrentEvidence(t *testing.T) {
	f, id, a, fence := startedMission(t)
	m := f.missions[id]
	// Evidence for an older generation: refused.
	mustStep(t, f, domain.Command{Kind: domain.CmdRecordEvidence, Mission: id, Attempt: a, Fence: fence,
		Revision: m.Version.IntentRevision, Generation: m.Version.CandidateGeneration - 1, Head: m.Head, Receipt: "rcpt_old"})
	_, err := f.step(t, domain.Command{Kind: domain.CmdAccept, Mission: id, Attempt: a})
	if !errors.Is(err, domain.ErrEvidenceStale) {
		t.Fatalf("accept with stale-generation evidence: want ErrEvidenceStale, got %v", err)
	}
	// Evidence for the right generation but another head: refused.
	mustStep(t, f, domain.Command{Kind: domain.CmdRecordEvidence, Mission: id, Attempt: a, Fence: fence,
		Revision: m.Version.IntentRevision, Generation: m.Version.CandidateGeneration, Head: "sha_other", Receipt: "rcpt_head"})
	if _, err = f.step(t, domain.Command{Kind: domain.CmdAccept, Mission: id, Attempt: a}); !errors.Is(err, domain.ErrEvidenceStale) {
		t.Fatalf("accept with other-head evidence: want ErrEvidenceStale, got %v", err)
	}
	// No evidence at all: refused.
	// Current evidence: accepted.
	mustStep(t, f, domain.Command{Kind: domain.CmdRecordEvidence, Mission: id, Attempt: a, Fence: fence,
		Revision: m.Version.IntentRevision, Generation: m.Version.CandidateGeneration, Head: m.Head, Receipt: "rcpt_now"})
	mustStep(t, f, domain.Command{Kind: domain.CmdAccept, Mission: id, Attempt: a})
	if f.missions[id].Candidate != domain.CandAccepted {
		t.Fatal("not accepted")
	}
	// A revised intent revokes the acceptance.
	mustStep(t, f, domain.Command{Kind: domain.CmdReviseIntent, Mission: id, Reason: "new scope"})
	if f.missions[id].Accepted != nil {
		t.Fatal("acceptance survived an intent revision")
	}
}

func TestDeliverNeedsCurrentGeneration(t *testing.T) {
	f, id, a, fence := startedMission(t)
	m := f.missions[id]
	mustStep(t, f, domain.Command{Kind: domain.CmdRecordEvidence, Mission: id, Attempt: a, Fence: fence,
		Revision: m.Version.IntentRevision, Generation: m.Version.CandidateGeneration, Head: m.Head, Receipt: "rcpt"})
	mustStep(t, f, domain.Command{Kind: domain.CmdAccept, Mission: id, Attempt: a})
	gen := m.Version.CandidateGeneration
	_, err := f.step(t, domain.Command{Kind: domain.CmdDeliver, Mission: id, Attempt: a, Generation: gen - 1})
	if !errors.Is(err, domain.ErrGenerationStale) {
		t.Fatalf("deliver old generation: want ErrGenerationStale, got %v", err)
	}
	if f.missions[id].DeliveredGeneration != nil {
		t.Fatal("stale delivery recorded")
	}
	mustStep(t, f, domain.Command{Kind: domain.CmdDeliver, Mission: id, Attempt: a, Generation: gen})
	if g := f.missions[id].DeliveredGeneration; g == nil || *g != gen {
		t.Fatalf("delivered generation = %v, want %d", g, gen)
	}
	// Selecting a new candidate clears the delivery binding (I6 holds).
	mustStep(t, f, domain.Command{Kind: domain.CmdSelectCandidate, Mission: id, Head: "sha_b"})
	if f.missions[id].DeliveredGeneration != nil {
		t.Fatal("delivered generation survived a new candidate generation")
	}
}

func TestUnknownAttemptBlocksRelaunch(t *testing.T) {
	f, id, a, fence := startedMission(t)
	mustStep(t, f, domain.Command{Kind: domain.CmdExpireLease, Mission: id, Attempt: a, Fence: fence})
	if f.missions[id].Attempts[a].State != domain.AttemptUnknown {
		t.Fatal("expired lease should be UNKNOWN")
	}
	_, err := f.step(t, domain.Command{Kind: domain.CmdStartAttempt, Mission: id, Attempt: "r_2", Host: "h_t2", Role: "writer"})
	if !errors.Is(err, domain.ErrUnknownUnresolved) {
		t.Fatalf("relaunch during UNKNOWN: want ErrUnknownUnresolved, got %v", err)
	}
	// A message with the dead fence is still refused after reconciliation.
	mustStep(t, f, domain.Command{Kind: domain.CmdReconcileAttempt, Mission: id, Attempt: a, Fence: fence, Result: "FAILED_DEFINITE"})
	ev := mustStep(t, f, domain.Command{Kind: domain.CmdStartAttempt, Mission: id, Attempt: "r_2", Host: "h_t2", Role: "writer"})
	if ev[0].Fence != fence+1 {
		t.Fatalf("new fence %d, want %d", ev[0].Fence, fence+1)
	}
	_, err = f.step(t, domain.Command{Kind: domain.CmdFinishAttempt, Mission: id, Attempt: a, Fence: fence, Outcome: "checkpoint"})
	if !errors.Is(err, domain.ErrInvalidTransition) && !errors.Is(err, domain.ErrStaleFence) {
		t.Fatalf("late message from the dead attempt: want a refusal, got %v", err)
	}
}

func TestApplyUnknownEventIsNoop(t *testing.T) {
	m := domain.NewMission("m_x")
	after := Apply(m, domain.Event{Kind: "FROM_THE_FUTURE", Mission: "m_x"})
	if Digest(after) != Digest(m) {
		t.Fatal("unknown event changed state")
	}
}

func TestRequestHashStable(t *testing.T) {
	a := domain.Command{Kind: domain.CmdWake, Mission: "m_1", Token: "t"}
	b := domain.Command{Kind: domain.CmdWake, Mission: "m_1", Token: "t"}
	if a.RequestHash() != b.RequestHash() {
		t.Fatal("equal commands hash differently")
	}
	b.Token = "u"
	if a.RequestHash() == b.RequestHash() {
		t.Fatal("different commands hash equal")
	}
}
