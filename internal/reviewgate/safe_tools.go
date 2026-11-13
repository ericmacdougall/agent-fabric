package reviewgate

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// EventSafeToolsApproved is the audit event of an operator approving that a
// mission runs with per-call review off for some tools (goodqa#15866).
const EventSafeToolsApproved = "SAFE_TOOLS_APPROVED"

// SafeToolsApproval is one operator's recorded decision to let a mission run
// without per-call human review for the tools its policy lists.
type SafeToolsApproval struct {
	MissionID    string
	PolicyDigest string // PolicyDigest of the exact policy the mission will run
	OperatorID   string // op:<name>, from the operator CLI
	Reason       string // required; only its digest is audited
}

// safeToolsScope is the audit row's request_hash. The audit chain keys rows by
// a 64-hex hash; this one is derived from the mission and the policy digest, so
// a row for another mission or another policy has another key.
func safeToolsScope(mission, digest string) string {
	return hashBytes([]byte("safe-tools/v1\x00" + mission + "\x00" + digest))
}

func safeToolsDetail(mission, digest string) string {
	return "mission=" + mission + ";policy_digest=" + digest + ";"
}

func validOperatorActor(actor string) bool {
	return strings.HasPrefix(actor, "op:") && len(actor) > len("op:") && validText(actor, MaxNameBytes) &&
		!strings.ContainsAny(actor, "\x00\r\n\t ")
}

// ApproveSafeTools appends the approval to the audit chain, with the
// operator, the time (the chain's own), the mission and the policy digest. It
// is an operator-surface call: no tool the model holds reaches it, and the
// database refuses audit writes from the agent and runner roles.
func (o *Operator) ApproveSafeTools(ctx context.Context, a SafeToolsApproval) (AuditEntry, error) {
	if !validOperatorActor(a.OperatorID) {
		return AuditEntry{}, invalid("safe_tools approval needs an op:<name> operator identity")
	}
	if !validText(a.MissionID, MaxNameBytes) || !validHex64(a.PolicyDigest) {
		return AuditEntry{}, invalid("safe_tools approval needs a mission id and a policy digest")
	}
	if !validText(a.Reason, MaxAuditText) {
		return AuditEntry{}, invalid("safe_tools approval needs a reason")
	}
	detail := safeToolsDetail(a.MissionID, a.PolicyDigest) + "reason_sha256=" + hashBytes([]byte(a.Reason))
	if len(detail) > MaxAuditText {
		return AuditEntry{}, invalid("safe_tools approval detail is too long")
	}
	tx, err := o.store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return AuditEntry{}, err
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck
	scope := safeToolsScope(a.MissionID, a.PolicyDigest)
	if err := appendAudit(ctx, tx, scope, EventSafeToolsApproved, a.OperatorID, detail, PolicyV1); err != nil {
		return AuditEntry{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AuditEntry{}, err
	}
	entry, ok, err := o.store.SafeToolsApproval(ctx, a.MissionID, a.PolicyDigest)
	if err != nil || !ok {
		return AuditEntry{}, fmt.Errorf("safe_tools approval recorded but not readable back: %v", err)
	}
	return entry, nil
}

// SafeToolsApproval returns the newest audit entry that records an operator
// approving safe_tools for exactly this mission and policy digest: the row's
// key is derived from both, so an entry for another mission or digest cannot
// match. One by a non-operator actor, or of another event, does not count.
func (s *PostgresStore) SafeToolsApproval(ctx context.Context, mission, digest string) (AuditEntry, bool, error) {
	if !validText(mission, MaxNameBytes) || !validHex64(digest) {
		return AuditEntry{}, false, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT seq, request_hash, event, actor, detail, policy_version, at, prev_hash, hash
		FROM review_gate_audit WHERE request_hash = $1 AND event = $2 ORDER BY seq DESC LIMIT 50`,
		safeToolsScope(mission, digest), EventSafeToolsApproved)
	if err != nil {
		return AuditEntry{}, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.Seq, &e.RequestHash, &e.Event, &e.Actor, &e.Detail,
			&e.PolicyVersion, &e.At, &e.PrevHash, &e.Hash); err != nil {
			return AuditEntry{}, false, err
		}
		// The row's key already names this mission and this digest.
		if validOperatorActor(e.Actor) {
			return e, true, nil
		}
	}
	return AuditEntry{}, false, rows.Err()
}
