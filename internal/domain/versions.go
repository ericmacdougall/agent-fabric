package domain

// Version is the three independent counters of a mission (DESIGN.md 3.4,
// adopted from the Kimi design and the Rust specification):
//
//   - IntentRevision changes only when the user's intent, scope or acceptance
//     criteria change. Reviews and verdicts bind to it.
//   - AttemptFence increases for every newly authorised attempt. A message that
//     carries an older fence is stale and may not mutate current state.
//   - CandidateGeneration identifies the output lineage: one per selected head
//     or delivery target. Evidence and delivery bind to it.
//
// There is deliberately no Less/Compare over the whole tuple: collapsing the
// three into one ordering is the defect the design names (plan invariant 2).
type Version struct {
	IntentRevision      int64 `json:"intent_revision"`
	AttemptFence        int64 `json:"attempt_fence"`
	CandidateGeneration int64 `json:"candidate_generation"`
}

// FenceIsStale reports whether a message carrying fence f is older than the
// mission's current fence. A fence equal to the current one is live.
func (v Version) FenceIsStale(f int64) bool { return f < v.AttemptFence }

// Regressed reports whether any single counter went backwards from prev to v.
// Used by the invariant checker, never by the reducer itself.
func (v Version) Regressed(prev Version) bool {
	return v.IntentRevision < prev.IntentRevision ||
		v.AttemptFence < prev.AttemptFence ||
		v.CandidateGeneration < prev.CandidateGeneration
}
