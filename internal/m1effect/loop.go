package m1effect

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"fabric/internal/domain"
	"fabric/internal/effects"
	"fabric/internal/records"
	"fabric/internal/store"
)

// Loop is fabd's effect reconcile loop (AC-31). Every Interval it settles
// what ended attempts left among the governed effects (the shared predicate:
// kind ticket_comment and idem_key equal to its mission's M1Key, on a
// database where the policy is enforced): it reconciles UNKNOWN, RECONCILING
// and stale DISPATCHED effects by readback, abandons a stranded
// never-dispatched INTENT (actor fabd, reason superseded), writes
// delivery_blocked against the ended attempt unless the effect ends
// CONFIRMED, and for 24 hours after a governed effect settles NOT_APPLIED
// takes a read-only readback that records late_write when its trailer
// appears. It never dispatches. It reads no mission config.
//
// A pass runs three bounded queries, each of at most Limit rows, so rows
// that never change state cannot starve the ones that need action:
// actionable rows (reconcile or abandon), record-only terminal rows (one
// delivery_blocked record each, skipped once written by this process), and
// late-write checks (each rechecked at most every LateWriteRecheck).
type Loop struct {
	Store      *store.Store
	Broker     *effects.Broker
	Exec       effects.Executor
	RecordsDir string
	Interval   time.Duration // default 60 s
	Limit      int           // per query; default 20
	// Timeout bounds one pass (default PassTimeout): the pass runs inside
	// the scheduler tick, so a hung GoodQA must not stall it.
	Timeout time.Duration
	// Recheck is the least gap between two late-write readbacks of one
	// effect (default LateWriteRecheck).
	Recheck time.Duration
	// Bump raises a durable counter (fabd's s.bump), e.g. m1_late_write_total.
	Bump func(ctx context.Context, name string)

	mu       sync.Mutex
	last     time.Time
	recorded map[string]time.Time // record-only effect id -> when written
	checked  map[string]time.Time // late-write effect id -> next check due
}

// LateWriteWindow is how long after settling NOT_APPLIED the loop keeps
// looking for a late write, and how long after settling a record-only
// terminal row stays selectable.
const LateWriteWindow = 24 * time.Hour

// LateWriteRecheck is the least gap between two late-write readbacks of the
// same effect.
const LateWriteRecheck = 5 * time.Minute

// PassTimeout is the default deadline of one pass.
const PassTimeout = 30 * time.Second

// governedSQL is the shared predicate in SQL; Go re-checks M1Key on every
// row it acts on.
const governedSQL = `kind = 'ticket_comment' AND idem_key = 'm1-' || left(encode(sha256(convert_to(mission_id, 'UTF8')), 'hex'), 24)`

const liveSQL = `EXISTS (SELECT 1 FROM attempts a WHERE a.id = effects.attempt_id AND a.state IN ('ADMITTED', 'LEASED', 'PREPARING', 'RUNNING'))`

const (
	actionableSQL = `(state IN ('UNKNOWN', 'RECONCILING')
		OR (state = 'DISPATCHED' AND NOT ` + liveSQL + `)
		OR (state IN ('INTENT', 'APPROVAL_REQUIRED', 'APPROVED') AND dispatches = 0 AND NOT ` + liveSQL + `))`
	recordOnlySQL = `state IN ('FAILED_DEFINITE', 'QUARANTINED', 'RESOLVED_NOT_APPLIED') AND NOT ` + liveSQL + `
		AND updated_at > now() - $2::interval AND NOT (id = ANY(coalesce($3::text[], ARRAY[]::text[])))`
	lateWriteSQL = `state = 'NOT_APPLIED' AND dispatches > 0
		AND updated_at > now() - $2::interval AND NOT (id = ANY(coalesce($3::text[], ARRAY[]::text[])))`
)

// MaybePass runs Pass when Interval has passed since the last one ended.
func (l *Loop) MaybePass(ctx context.Context) {
	iv := l.Interval
	if iv <= 0 {
		iv = 60 * time.Second
	}
	l.mu.Lock()
	due := l.last.IsZero() || time.Since(l.last) >= iv
	l.mu.Unlock()
	if !due {
		return
	}
	if _, err := l.Pass(ctx); err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "fabd: m1 effect loop:", err)
	}
	l.mu.Lock()
	l.last = time.Now()
	l.mu.Unlock()
}

// PassStats counts what one pass did.
type PassStats struct {
	Reconciled, Abandoned, Blocked, LateWrites int
}

type row struct {
	id, key, state string
	mission        domain.MissionID
	attempt        domain.AttemptID
	dispatches     int
	live           bool
}

// Pass is one bounded pass.
func (l *Loop) Pass(ctx context.Context) (PassStats, error) {
	var ps PassStats
	to := l.Timeout
	if to <= 0 {
		to = PassTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()
	on, err := l.Broker.PolicyEnforced(ctx)
	if err != nil || !on {
		return ps, err
	}
	limit := l.Limit
	if limit <= 0 {
		limit = 20
	}
	window := fmt.Sprintf("%d seconds", int(LateWriteWindow.Seconds()))
	recordedIDs, dueIDs := l.exclusions()

	act, err := l.query(ctx, actionableSQL+` ORDER BY updated_at LIMIT $1`, limit)
	if err != nil {
		return ps, err
	}
	for _, r := range act {
		if ctx.Err() != nil {
			return ps, ctx.Err()
		}
		l.one(ctx, r, &ps)
	}
	rec, err := l.query(ctx, recordOnlySQL+` ORDER BY updated_at LIMIT $1`, limit, window, recordedIDs)
	if err != nil {
		return ps, err
	}
	for _, r := range rec {
		l.blocked(r, stateClass(effects.State(r.state)), &ps)
		l.mu.Lock()
		l.recorded[r.id] = time.Now()
		l.mu.Unlock()
	}
	late, err := l.query(ctx, lateWriteSQL+` ORDER BY updated_at LIMIT $1`, limit, window, dueIDs)
	if err != nil {
		return ps, err
	}
	for _, r := range late {
		if ctx.Err() != nil {
			return ps, ctx.Err()
		}
		l.mu.Lock()
		rc := l.Recheck
		if rc <= 0 {
			rc = LateWriteRecheck
		}
		l.checked[r.id] = time.Now().Add(rc)
		l.mu.Unlock()
		l.one(ctx, r, &ps)
	}
	return ps, nil
}

// exclusions prunes the in-process sets and returns the ids to skip: record
// rows already written, and late-write rows not yet due for a recheck. A
// restarted fabd starts empty; records are idempotent, so that only repeats
// identical work.
func (l *Loop) exclusions() (recorded, notDue []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.recorded == nil {
		l.recorded = map[string]time.Time{}
	}
	if l.checked == nil {
		l.checked = map[string]time.Time{}
	}
	now := time.Now()
	for id, at := range l.recorded {
		if now.Sub(at) > LateWriteWindow+time.Hour {
			delete(l.recorded, id)
		} else {
			recorded = append(recorded, id)
		}
	}
	for id, due := range l.checked {
		if now.Sub(due) > LateWriteWindow+time.Hour {
			delete(l.checked, id)
		} else if now.Before(due) {
			notDue = append(notDue, id)
		}
	}
	return recorded, notDue
}

func (l *Loop) query(ctx context.Context, where string, args ...any) ([]row, error) {
	rows, err := l.Store.Pool().Query(ctx, `SELECT id, idem_key, state, mission_id, attempt_id, dispatches, `+liveSQL+` FROM effects
		WHERE `+governedSQL+` AND `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var page []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.key, &r.state, &r.mission, &r.attempt, &r.dispatches, &r.live); err != nil {
			return nil, err
		}
		if r.key != effects.M1Key(r.mission) {
			continue // defence in depth: the predicate is the Go function
		}
		page = append(page, r)
	}
	return page, rows.Err()
}

func (l *Loop) blocked(r row, class string, ps *PassStats) {
	writeRecord(l.RecordsDir, records.Record{Mission: r.mission, Attempt: r.attempt, Kind: records.DeliveryBlocked, Class: cleanClass(class), Effect: r.id})
	ps.Blocked++
}

func (l *Loop) one(ctx context.Context, r row, ps *PassStats) {
	switch effects.State(r.state) {
	case effects.Unknown, effects.Reconciling, effects.Dispatched:
		if effects.State(r.state) == effects.Dispatched && r.live {
			return // its dispatcher is still in charge
		}
		e, err := l.Broker.Reconcile(ctx, l.Exec, r.id)
		if err != nil {
			if !errors.Is(err, effects.ErrNotReconcilable) {
				fmt.Fprintf(os.Stderr, "fabd: m1 reconcile %s: %v\n", r.id, err)
			}
			return
		}
		ps.Reconciled++
		switch e.State {
		case effects.Confirmed, effects.Unknown:
			// CONFIRMED: delivered. UNKNOWN: a later pass tries again.
		default:
			if !r.live {
				l.blocked(r, stateClass(e.State), ps)
			}
		}
	case effects.Intent, effects.ApprovalRequired, effects.Approved:
		if _, err := l.Broker.Abandon(ctx, r.id, "superseded", "fabd:m1-effect-loop", "fabd"); err != nil {
			fmt.Fprintf(os.Stderr, "fabd: m1 abandon %s: %v\n", r.id, err)
			return
		}
		ps.Abandoned++
		l.blocked(r, "superseded", ps)
	case effects.FailedDefinite, effects.Quarantined, effects.ResolvedNotApplied:
		l.blocked(r, stateClass(effects.State(r.state)), ps)
	case effects.NotApplied:
		if !r.live {
			l.blocked(r, stateClass(effects.NotApplied), ps)
		}
		f, ok := l.Exec.(effects.CommentFinder)
		if !ok {
			return
		}
		e, err := l.Broker.Load(ctx, r.id)
		if err != nil {
			return
		}
		if prev, _ := records.Read(l.RecordsDir, r.attempt); l.RecordsDir != "" {
			for _, x := range prev {
				if x.Kind == records.LateWrite && x.Effect == r.id {
					return // already recorded and counted
				}
			}
		}
		id, found, err := f.FindComment(ctx, e)
		if err != nil || !found {
			return
		}
		writeRecord(l.RecordsDir, records.Record{Mission: r.mission, Attempt: r.attempt, Kind: records.LateWrite, CommentID: cleanRemote(id), Effect: r.id})
		l.mu.Lock()
		l.checked[r.id] = time.Now().Add(LateWriteWindow)
		l.mu.Unlock()
		ps.LateWrites++
		if l.Bump != nil {
			l.Bump(ctx, "m1_late_write_total")
		}
	}
}

func cleanRemote(id string) string {
	b := []byte(id)
	for i, ch := range b {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || strings.IndexByte("._:-", ch) >= 0) {
			b[i] = '_'
		}
	}
	if len(b) > 80 {
		b = b[:80]
	}
	if len(b) == 0 {
		return "unknown"
	}
	return string(b)
}
