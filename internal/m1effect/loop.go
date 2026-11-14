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

// Loop is fabd's effect reconcile loop (AC-31). Every Interval it takes up
// to Limit governed effects (the shared predicate: kind ticket_comment and
// idem_key equal to its mission's M1Key, on a database where the policy is
// enforced) and settles what ended attempts left: it reconciles UNKNOWN,
// RECONCILING and stale DISPATCHED effects by readback, abandons a stranded
// never-dispatched INTENT (actor fabd, reason superseded), writes
// delivery_blocked against the ended attempt unless the effect ends
// CONFIRMED, and for 24 hours after a governed effect settles NOT_APPLIED
// takes a read-only readback that records late_write when its trailer
// appears. It never dispatches. It reads no mission config.
type Loop struct {
	Store      *store.Store
	Broker     *effects.Broker
	Exec       effects.Executor
	RecordsDir string
	Interval   time.Duration // default 60 s
	Limit      int           // default 20
	// Bump raises a durable counter (fabd's s.bump), e.g. m1_late_write_total.
	Bump func(ctx context.Context, name string)

	mu   sync.Mutex
	last time.Time
}

// LateWriteWindow is how long after settling NOT_APPLIED the loop keeps
// looking for a late write.
const LateWriteWindow = 24 * time.Hour

// governedSQL is the shared predicate in SQL; Go re-checks M1Key on every
// row it acts on.
const governedSQL = `kind = 'ticket_comment' AND idem_key = 'm1-' || left(encode(sha256(convert_to(mission_id, 'UTF8')), 'hex'), 24)`

const liveSQL = `EXISTS (SELECT 1 FROM attempts a WHERE a.id = effects.attempt_id AND a.state IN ('ADMITTED', 'LEASED', 'PREPARING', 'RUNNING'))`

// MaybePass runs Pass when Interval has passed since the last one.
func (l *Loop) MaybePass(ctx context.Context) {
	iv := l.Interval
	if iv <= 0 {
		iv = 60 * time.Second
	}
	l.mu.Lock()
	due := l.last.IsZero() || time.Since(l.last) >= iv
	if due {
		l.last = time.Now()
	}
	l.mu.Unlock()
	if due {
		if _, err := l.Pass(ctx); err != nil && ctx.Err() == nil {
			fmt.Fprintln(os.Stderr, "fabd: m1 effect loop:", err)
		}
	}
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
	on, err := l.Broker.PolicyEnforced(ctx)
	if err != nil || !on {
		return ps, err
	}
	limit := l.Limit
	if limit <= 0 {
		limit = 20
	}
	rows, err := l.Store.Pool().Query(ctx, `SELECT id, idem_key, state, mission_id, attempt_id, dispatches, `+liveSQL+` FROM effects
		WHERE `+governedSQL+` AND (
			state IN ('UNKNOWN', 'RECONCILING', 'DISPATCHED')
			OR (state IN ('INTENT', 'APPROVAL_REQUIRED', 'APPROVED') AND dispatches = 0 AND NOT `+liveSQL+`)
			OR (state IN ('FAILED_DEFINITE', 'QUARANTINED', 'RESOLVED_NOT_APPLIED') AND NOT `+liveSQL+`)
			OR (state = 'NOT_APPLIED' AND dispatches > 0 AND updated_at > now() - $2::interval))
		ORDER BY updated_at LIMIT $1`, limit, fmt.Sprintf("%d seconds", int(LateWriteWindow.Seconds())))
	if err != nil {
		return ps, err
	}
	var page []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.key, &r.state, &r.mission, &r.attempt, &r.dispatches, &r.live); err != nil {
			rows.Close()
			return ps, err
		}
		page = append(page, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return ps, err
	}
	for _, r := range page {
		if r.key != effects.M1Key(r.mission) {
			continue // defence in depth: the predicate is the Go function
		}
		l.one(ctx, r, &ps)
	}
	return ps, nil
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
