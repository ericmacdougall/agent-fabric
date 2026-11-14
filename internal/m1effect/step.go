// Package m1effect is fabd's side of the M1 result effect (goodqa#15966
// AC-7, AC-11, AC-29, AC-31): the pre-submit step that posts the mission's
// one comment while its attempt is live, and the reconcile loop that settles
// what an ended attempt left behind. The broker enforces the comment policy;
// this package never decides identity by mission config or key prefix.
package m1effect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"fabric/internal/domain"
	"fabric/internal/effects"
	"fabric/internal/m1comment"
	"fabric/internal/records"
	"fabric/internal/store"
)

// MinLease is the least lease the effect step needs left before it starts
// (AC-11): Precondition plus transport take at most 60 s.
const MinLease = 120 * time.Second

// Attempt names the attempt whose stream has just settled.
type Attempt struct {
	Mission domain.MissionID
	Attempt domain.AttemptID
	Fence   int64
	RunDir  string
	// Outcome is the settled stream's outcome; the step runs only after an
	// EndCheckpoint settled to a non-abandoned outcome.
	Outcome string
}

// Delivery is what the comment needs, assembled from the mission config
// (W5: issue id, ticket, extract file list), the redacted stream (W3: the
// model's result text) and the attempt's bound credentials (W3).
type Delivery struct {
	IssueID string
	Ticket  string
	Fields  m1comment.Fields
	Result  string
	Allow   m1comment.Allow
	Creds   m1comment.BoundCredentials
}

// Source assembles a Delivery for an attempt. An error carrying a class
// (ClassError) blocks delivery with that class, for example
// credential_material_unavailable; any other error blocks it as
// prepare_failed.
type Source interface {
	Prepare(ctx context.Context, a Attempt) (Delivery, error)
}

// ClassError is a Source error with a fixed delivery_blocked class.
type ClassError struct {
	Class string
	Err   error
}

func (e *ClassError) Error() string { return e.Class + ": " + e.Err.Error() }
func (e *ClassError) Unwrap() error { return e.Err }

// Result kinds.
const (
	Delivered        = "delivered"
	DeliveredEarlier = "delivered_earlier"
	Blocked          = "delivery_blocked"
	Pending          = "pending" // dispatched, outcome unknown: AC-31 settles it
	Skipped          = "skipped" // no EndCheckpoint result: nothing to post
)

// Result is what the step did.
type Result struct {
	Kind   string
	Class  string
	Effect string
}

// Prefix is the FINISH reason prefix (AC-29), or "".
func (r Result) Prefix() string {
	if r.Kind == Blocked {
		return "m1:delivery_blocked:" + r.Class
	}
	return ""
}

// Step is the pre-submit step.
type Step struct {
	Store      *store.Store
	Broker     *effects.Broker
	Exec       effects.Executor
	Source     Source
	RecordsDir string
	// beforeSubmit is a test seam (AC-8 cutpoint "before Submit").
	beforeSubmit func()
	// afterSubmit is a test seam (AC-8 cutpoint "after the INTENT commit").
	afterSubmit func(effects.Effect)
}

// settledOutcome: the stream ended with a result fabd may publish.
func settledOutcome(o string) bool {
	switch o {
	case "checkpoint", "ready_for_review", "blocked", "wait":
		return true
	}
	return false
}

// Run is the pre-submit step: after the stream ended with an EndCheckpoint,
// while the attempt is live, before FINISH_ATTEMPT. heartbeat renews the
// attempt's lease. It returns what it did; a blocked delivery is also
// written as a record. Run never submits a second effect for a mission.
func (s *Step) Run(ctx context.Context, a Attempt, heartbeat func(context.Context) error) Result {
	if !settledOutcome(a.Outcome) {
		return Result{Kind: Skipped}
	}
	key := effects.M1Key(a.Mission)
	// AC-11: renew the lease, then refuse to start with < 120 s left.
	if heartbeat != nil {
		if err := heartbeat(ctx); err != nil {
			return s.block(a, "heartbeat_failed", "")
		}
	}
	left, err := s.leaseLeft(ctx, a.Attempt)
	if err != nil || left < MinLease {
		return s.block(a, "lease_budget", "")
	}
	existing, err := s.Broker.LoadKey(ctx, key)
	switch {
	case err == nil:
		switch existing.State {
		case effects.Confirmed, effects.ResolvedApplied:
			s.record(records.Record{Mission: a.Mission, Attempt: a.Attempt, Kind: records.DeliveredEarlier, Effect: existing.ID})
			return Result{Kind: DeliveredEarlier, Effect: existing.ID}
		default:
			return s.block(a, stateClass(existing.State), existing.ID)
		}
	case !errors.Is(err, pgx.ErrNoRows):
		return s.block(a, "lookup_failed", "")
	}
	d, err := s.Source.Prepare(ctx, a)
	if err != nil {
		var ce *ClassError
		if errors.As(err, &ce) {
			return s.block(a, ce.Class, "")
		}
		return s.block(a, "prepare_failed", "")
	}
	if blocked, class := m1comment.Scan(d.Result, d.Creds, d.Allow); blocked {
		return s.block(a, class, "")
	}
	body, err := m1comment.Body(d.Fields, d.Result)
	if err != nil {
		return s.block(a, "body_invalid", "")
	}
	req, _ := json.Marshal(map[string]string{"issue_id": d.IssueID, "ticket": d.Ticket, "body": body})
	if s.beforeSubmit != nil {
		s.beforeSubmit()
	}
	e, err := s.Broker.Submit(ctx, s.Exec, effects.IntentSpec{Mission: a.Mission, Attempt: a.Attempt, Fence: a.Fence,
		Kind: effects.GovernedKind, Subject: "ticket:" + d.Ticket, IdemKey: key, Request: req})
	if err != nil {
		var pr *effects.PolicyRefusal
		if errors.As(err, &pr) {
			return s.block(a, pr.Code, "")
		}
		if errors.Is(err, domain.ErrIdempotencyConflict) {
			return s.block(a, "idempotency_conflict", "")
		}
		return s.block(a, "submit_failed", "")
	}
	if s.afterSubmit != nil {
		s.afterSubmit(e)
	}
	if e.Replayed {
		// A concurrent writer stored the key between the lookup and Submit.
		return s.block(a, stateClass(e.State), e.ID)
	}
	e, err = s.Broker.Dispatch(ctx, s.Exec, e.ID)
	if err != nil {
		var pr *effects.PolicyRefusal
		if errors.As(err, &pr) {
			return s.block(a, pr.Code, e.ID)
		}
		// The outcome is recorded by the broker or left DISPATCHED for the
		// AC-31 loop; never retried here.
		return Result{Kind: Pending, Effect: e.ID}
	}
	switch e.State {
	case effects.Confirmed:
		return Result{Kind: Delivered, Effect: e.ID}
	case effects.Unknown, effects.Dispatched, effects.Reconciling:
		return Result{Kind: Pending, Effect: e.ID}
	default:
		return s.block(a, stateClass(e.State), e.ID)
	}
}

// stateClass is the delivery_blocked class for an effect left in state st.
func stateClass(st effects.State) string {
	return "effect_" + strings.ToLower(string(st))
}

func (s *Step) block(a Attempt, class, effect string) Result {
	class = cleanClass(class)
	s.record(records.Record{Mission: a.Mission, Attempt: a.Attempt, Kind: records.DeliveryBlocked, Class: class, Effect: effect})
	return Result{Kind: Blocked, Class: class, Effect: effect}
}

func (s *Step) record(r records.Record) { writeRecord(s.RecordsDir, r) }

func writeRecord(dir string, r records.Record) {
	if dir == "" {
		return
	}
	if err := records.Append(dir, r); err != nil {
		fmt.Fprintf(os.Stderr, "fabd: m1 record %s for %s not written: %v\n", r.Kind, r.Attempt, err)
	}
}

// cleanClass maps a class onto the records grammar (lower case, at most 80
// bytes, [a-z0-9_.:-]); a class is a code, so nothing meaningful is lost.
func cleanClass(c string) string {
	c = strings.ToLower(c)
	b := []byte(c)
	for i, ch := range b {
		if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '_' || ch == '.' || ch == ':' || ch == '-') {
			b[i] = '_'
		}
	}
	if len(b) > 80 {
		b = b[:80]
	}
	if len(b) == 0 || !(b[0] >= 'a' && b[0] <= 'z' || b[0] >= '0' && b[0] <= '9') {
		return "unclassified"
	}
	return string(b)
}

func (s *Step) leaseLeft(ctx context.Context, attempt domain.AttemptID) (time.Duration, error) {
	var secs *float64
	if err := s.Store.Pool().QueryRow(ctx, `SELECT extract(epoch FROM lease_until - now())::float8 FROM attempts WHERE id = $1`,
		string(attempt)).Scan(&secs); err != nil {
		return 0, err
	}
	if secs == nil {
		return 0, errors.New("attempt has no lease")
	}
	return time.Duration(*secs * float64(time.Second)), nil
}
