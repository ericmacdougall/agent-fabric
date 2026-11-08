package domain

import "errors"

// Stable refusal reasons. A refusal is a recorded response, not a crash: the
// store writes it under the command's idempotency key so a retry sees the same
// answer (DESIGN.md 3.1, lost-response protocol).
var (
	ErrInvalidID           = errors.New("invalid id")
	ErrNotFound            = errors.New("not found")
	ErrStaleFence          = errors.New("stale fence")
	ErrIdempotencyConflict = errors.New("idempotency conflict: same key, different body")
	ErrInvalidTransition   = errors.New("invalid transition")
	ErrOverCapacity        = errors.New("over capacity")
	ErrUnknownUnresolved   = errors.New("effect UNKNOWN and unresolved: retry refused")
	ErrEvidenceStale       = errors.New("evidence not bound to the current revision, generation and head")
	ErrGenerationStale     = errors.New("generation is not the current candidate generation")
	ErrAttemptLive         = errors.New("attempt already live")
	ErrDuplicate           = errors.New("duplicate")
	// ErrLiveTestMissing (AC-6, R6): done needs a live_test receipt for the
	// deployed commit; merged or deployed is not done.
	ErrLiveTestMissing = errors.New("done needs a live_test receipt for the deployed commit")
	// ErrUnsupportedEvent: the log holds an event kind this build cannot interpret.
	ErrUnsupportedEvent = errors.New("unsupported event kind")
	// goodqa#15324 S1b ladder refusals.
	ErrInvalidReceipt   = errors.New("invalid ladder record")
	ErrNotLadderMission = errors.New("ladder records go only on a ladder mission")
	ErrLadderMission    = errors.New("a ladder mission accepts only ladder records")
	ErrTooManyReceipts  = errors.New("too many ladder receipts")
)
