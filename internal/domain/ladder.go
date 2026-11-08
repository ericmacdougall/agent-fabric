package domain

import (
	"fmt"
	"regexp"
	"strings"
)

// goodqa#15324 S1b: harness model-ladder receipts as Fabric events.
//
// A ladder mission (m_ladder-<task>) is a per-task log of LADDER_RECEIPT
// events. escalation.py classifies receipts; Fabric validates the SHAPE of an
// already-classified record, stores it append-only and folds it through
// internal/ladder. Nothing here derives a class.
//
// Compatibility (PL-2): the 9abc522 OutboxPage decodes every outbox payload
// into its Event and a decode error stops fabd. So all ladder data nests under
// the single top-level key `ladder`, and no nested name equals (ignoring case)
// an old Event or Command key; TestLadderKeysNeverCollideWithOldEventOrCommandKeys
// pins that.

const (
	// LadderMissionPrefix: ParseMissionID needs the m_ prefix and at most 64
	// bytes, so the id is m_ladder-<task> and the task is at most 55 bytes.
	LadderMissionPrefix = "m_ladder-"
	// LadderTicket is the fixed ticket of every ladder mission row.
	LadderTicket = "ladder-receipts"
	// LadderActor is the one actor that writes ladder records.
	LadderActor ActorID = "ladder"
	// MaxLadderReceipts equals escalation.py MAX_EVENTS. Only LADDER_RECEIPT
	// events count toward it.
	MaxLadderReceipts = 4096
	// MaxLadderApprovals bounds LADDER_TOP_APPROVED events per task.
	MaxLadderApprovals = 64
)

var (
	ladderTask   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`) // escalation.py SAFE_KEY
	ladderFp     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	ladderSha    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	ladderCommit = regexp.MustCompile(`^[0-9a-f]{7,64}$`)
	ladderSrc    = regexp.MustCompile(`^[A-Za-z0-9._:/-]{0,64}$`)
	ladderNamed  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._-]{0,63}$`) // escalation.py NAMED
	ladderAsk    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)
	ladderBy     = regexp.MustCompile(`^op:[A-Za-z0-9][A-Za-z0-9._-]{0,59}$`)
)

// LadderMissionID returns the mission id for a task. A task that does not fit
// is refused, never truncated.
//
// Single-tenant: id m_ladder-<task> has no tenant/org/harness scope; actor "ladder"/"op:*" unscoped; rows sit on provider anthropic-api. Advisory until S3; re-scoping is a forward migration of ladder rows only (roadmap).
func LadderMissionID(task string) (MissionID, error) {
	if !ladderTask.MatchString(task) {
		return "", fmt.Errorf("%w: ladder task is not a SAFE_KEY", ErrInvalidID)
	}
	id := LadderMissionPrefix + task
	if len(id) > maxIDLen {
		return "", fmt.Errorf("%w: ladder task is longer than %d bytes", ErrInvalidID, maxIDLen-len(LadderMissionPrefix))
	}
	return MissionID(id), nil
}

// IsLadderMission is true for exactly the ids that start with the prefix.
func IsLadderMission(id MissionID) bool { return strings.HasPrefix(string(id), LadderMissionPrefix) }

// LadderTask returns the task of a ladder mission id.
func LadderTask(id MissionID) string { return strings.TrimPrefix(string(id), LadderMissionPrefix) }

// LadderApproval is the payload of an approve-top record. Fabric checks the
// shape of the source only; it does not resolve the ask (advisory until S3).
type LadderApproval struct {
	Ask string `json:"ask,omitempty"`
	Src string `json:"src,omitempty"`
	By  string `json:"by,omitempty"` // operator identity, op:*
}

// LadderRecord is the single nested payload. Every field is omitempty and no
// name equals an old Event or Command key, ignoring case.
type LadderRecord struct {
	Class       string          `json:"class,omitempty"`        // FAIL | CLAIM | PASS | NOT_COUNTED, from escalation.py
	Step        string          `json:"step,omitempty"`         // review | test | spec_review | claim | spend | approve_top
	Fp          *string         `json:"fp,omitempty"`           // 64 lowercase hex; non-nil iff class FAIL
	Commit      *string         `json:"commit,omitempty"`       // 7-64 lowercase hex
	Rsha        *string         `json:"rsha,omitempty"`         // receipt sha256
	Src         string          `json:"src,omitempty"`          // escalation.py source label
	Launched    bool            `json:"launched,omitempty"`     // a provider launch happened
	Counted     bool            `json:"counted,omitempty"`      // Python counted a launch against the ceiling
	SpendMicros int64           `json:"spend_micros,omitempty"` // USD millionths Python added for this record
	Ordinal     int64           `json:"ord,omitempty"`          // spend steps: per-task source ordinal (PL2-1)
	IDs         []string        `json:"ids,omitempty"`          // display only: named finding ids
	Approval    *LadderApproval `json:"approval,omitempty"`
}

// Clone copies the record so a stored event never aliases a command.
func (r *LadderRecord) Clone() *LadderRecord {
	if r == nil {
		return nil
	}
	c := *r
	if r.Fp != nil {
		v := *r.Fp
		c.Fp = &v
	}
	if r.Commit != nil {
		v := *r.Commit
		c.Commit = &v
	}
	if r.Rsha != nil {
		v := *r.Rsha
		c.Rsha = &v
	}
	c.IDs = append([]string(nil), r.IDs...)
	if r.Approval != nil {
		a := *r.Approval
		c.Approval = &a
	}
	return &c
}

func bad(why string) error { return fmt.Errorf("%w: %s", ErrInvalidReceipt, why) }

// ValidateLadderRecord checks the shape of a classified record. Messages name
// the rule, never the offending value.
func ValidateLadderRecord(r *LadderRecord) error {
	if r == nil {
		return bad("no ladder record")
	}
	if r.Src != "" && !ladderSrc.MatchString(r.Src) {
		return bad("src is not a plain label")
	}
	if r.Step == "approve_top" {
		return validateApproval(r)
	}
	if r.Approval != nil {
		return bad("approval only on approve_top")
	}
	switch r.Step {
	case "review", "test", "spec_review":
		if r.Class != "FAIL" && r.Class != "PASS" && r.Class != "NOT_COUNTED" {
			return bad("class must be FAIL, PASS or NOT_COUNTED")
		}
		if r.Rsha == nil || !ladderSha.MatchString(*r.Rsha) {
			return bad("rsha must be 64 lowercase hex")
		}
		if r.Ordinal != 0 {
			return bad("ord only on spend")
		}
	case "claim":
		if r.Class != "CLAIM" {
			return bad("claim step must have class CLAIM")
		}
		if r.Commit == nil {
			return bad("claim needs a commit")
		}
		if r.Rsha != nil || r.SpendMicros != 0 || r.Counted || r.Launched || r.Ordinal != 0 {
			return bad("claim carries only a commit")
		}
	case "spend":
		if r.Class != "NOT_COUNTED" {
			return bad("spend step must have class NOT_COUNTED")
		}
		if r.SpendMicros <= 0 {
			return bad("spend step needs spend_micros > 0")
		}
		if r.Ordinal < 1 || r.Ordinal > 1_000_000_000 {
			return bad("spend step needs ord in 1..1e9")
		}
		if r.Rsha != nil || r.Commit != nil || r.Counted || r.Launched || r.Fp != nil {
			return bad("spend carries only spend_micros and ord")
		}
	default:
		return bad("unknown step")
	}
	if (r.Class == "FAIL") != (r.Fp != nil) {
		return bad("fp is required for FAIL and forbidden otherwise")
	}
	if r.Fp != nil && !ladderFp.MatchString(*r.Fp) {
		return bad("fp must be 64 lowercase hex")
	}
	if r.Commit != nil && !ladderCommit.MatchString(*r.Commit) {
		return bad("commit must be 7-64 lowercase hex")
	}
	if r.Step == "review" && r.Class == "FAIL" && !r.Launched {
		return bad("a FAIL review needs a launch")
	}
	if r.Counted && (!r.Launched || r.Step != "review" || r.Class == "PASS") {
		return bad("counted needs a launched, non-PASS review")
	}
	if r.SpendMicros < 0 || r.SpendMicros > 1_000_000_000_000 {
		return bad("spend_micros out of range")
	}
	if r.Step != "spend" && r.SpendMicros != 0 && !r.Counted {
		return bad("spend_micros only on a counted receipt or a spend step")
	}
	if len(r.IDs) > 32 {
		return bad("too many ids")
	}
	for _, id := range r.IDs {
		if !ladderNamed.MatchString(id) || strings.EqualFold(id, "UNSPECIFIED") {
			return bad("ids must be named finding ids")
		}
	}
	return nil
}

// approvalSourceKinds are the source kinds a user can stand behind. ticket:,
// wal:, file:, inferred:, pasted: and agent text never approve the top rung.
var approvalSourceKinds = []string{"user:", "answer:", "queue:"}

func validateApproval(r *LadderRecord) error {
	if r.Class != "" || r.Fp != nil || r.Commit != nil || r.Rsha != nil || r.Launched || r.Counted ||
		r.SpendMicros != 0 || r.Ordinal != 0 || len(r.IDs) != 0 {
		return bad("approve_top carries only an approval")
	}
	a := r.Approval
	if a == nil {
		return bad("approve_top needs an approval")
	}
	if !ladderBy.MatchString(a.By) {
		return bad("approval must name an operator actor op:*")
	}
	if !ladderAsk.MatchString(a.Ask) {
		return bad("approval needs an ask id")
	}
	ok := false
	for _, k := range approvalSourceKinds {
		if strings.HasPrefix(a.Src, k) && len(a.Src) > len(k) {
			ok = true
		}
	}
	if !ok || len(a.Src) > 256 {
		return bad("approval needs a user:, answer: or queue: source")
	}
	for _, c := range a.Src {
		if c < 0x20 || c == 0x7f {
			return bad("approval source has control characters")
		}
	}
	return nil
}

// LadderEventKind is the event a valid record produces.
func LadderEventKind(r *LadderRecord) EventKind {
	if r.Step == "approve_top" {
		return EvLadderTopApproved
	}
	return EvLadderReceipt
}
