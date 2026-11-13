package reviewgate

import (
	"strings"
	"testing"
	"time"
)

// goodqa#15866 (C3): turning on safe_tools needs a recorded operator approval
// for this mission and this exact policy.
func TestSafeToolsApprovalIsRecordedAndMatchedExactly(t *testing.T) {
	st, ctx := openTestStore(t)
	op, _ := NewOperator(st)
	digest := hex64("policy A")
	other := hex64("policy B")

	if _, ok, err := st.SafeToolsApproval(ctx, "m_1", digest); err != nil || ok {
		t.Fatalf("an approval exists before any was recorded: %v %v", ok, err)
	}
	// only an op:<name> identity may record one
	for _, bad := range []string{"", "agent:s_1", "op:", "alice", "op: alice", "op:a\nb"} {
		if _, err := op.ApproveSafeTools(ctx, SafeToolsApproval{MissionID: "m_1", PolicyDigest: digest, OperatorID: bad, Reason: "r"}); err == nil {
			t.Errorf("actor %q recorded a safe_tools approval", bad)
		}
	}
	for name, a := range map[string]SafeToolsApproval{
		"no reason":      {MissionID: "m_1", PolicyDigest: digest, OperatorID: "op:alice"},
		"bad digest":     {MissionID: "m_1", PolicyDigest: "abc", OperatorID: "op:alice", Reason: "r"},
		"empty mission":  {PolicyDigest: digest, OperatorID: "op:alice", Reason: "r"},
		"uppercase hash": {MissionID: "m_1", PolicyDigest: strings.ToUpper(digest), OperatorID: "op:alice", Reason: "r"},
	} {
		if _, err := op.ApproveSafeTools(ctx, a); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, ok, _ := st.SafeToolsApproval(ctx, "m_1", digest); ok {
		t.Fatal("a refused approval counted")
	}

	before := time.Now().Add(-2 * time.Second)
	e, err := op.ApproveSafeTools(ctx, SafeToolsApproval{MissionID: "m_1", PolicyDigest: digest, OperatorID: "op:alice", Reason: "reviewed the grant"})
	if err != nil {
		t.Fatal(err)
	}
	// the actor, the time, the mission and the digest are in the audit chain
	if e.Actor != "op:alice" || e.Event != EventSafeToolsApproved || e.At.Before(before) || e.At.After(time.Now().Add(2*time.Second)) ||
		!strings.Contains(e.Detail, "mission=m_1;") || !strings.Contains(e.Detail, "policy_digest="+digest) ||
		strings.Contains(e.Detail, "reviewed the grant") || !strings.Contains(e.Detail, "reason_sha256=") {
		t.Fatalf("the recorded approval is incomplete: %+v", e)
	}
	if err := st.VerifyAudit(ctx); err != nil {
		t.Fatalf("audit chain: %v", err)
	}
	got, ok, err := st.SafeToolsApproval(ctx, "m_1", digest)
	if err != nil || !ok || got.Seq != e.Seq {
		t.Fatalf("the matching approval is not found: %+v %v %v", got, ok, err)
	}
	// a different digest, a different mission: not approved
	if _, ok, _ := st.SafeToolsApproval(ctx, "m_1", other); ok {
		t.Fatal("an approval for another policy digest counted")
	}
	if _, ok, _ := st.SafeToolsApproval(ctx, "m_2", digest); ok {
		t.Fatal("an approval for another mission counted")
	}
	// a row with the right key but written by a non-operator actor does not count
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	forged := safeToolsScope("m_3", digest)
	if err := appendAudit(ctx, tx, forged, EventSafeToolsApproved, "agent:attacker", safeToolsDetail("m_3", digest), PolicyV1); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.SafeToolsApproval(ctx, "m_3", digest); ok {
		t.Fatal("a non-operator actor's row counted as an approval")
	}
	// another event under the same key does not count either
	tx, _ = st.pool.Begin(ctx)
	if err := appendAudit(ctx, tx, safeToolsScope("m_4", digest), "HUMAN_APPROVED", "op:alice", safeToolsDetail("m_4", digest), PolicyV1); err != nil {
		t.Fatal(err)
	}
	_ = tx.Commit(ctx)
	if _, ok, _ := st.SafeToolsApproval(ctx, "m_4", digest); ok {
		t.Fatal("another event counted as a safe_tools approval")
	}
}
