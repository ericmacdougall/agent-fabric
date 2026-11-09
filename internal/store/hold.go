package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"fabric/internal/domain"
	"fabric/internal/envelope"
)

// A held mission is one whose stored event this build cannot decode (docs/
// envelope.md). The hold is derived data in the existing outbox_cursors table
// (consumer "sched.hold.<mission>"), so no schema changes: deploy-live.sh and
// the binary-only rollback both apply. The scheduler cursor always advances;
// the held mission's later rows are skipped on the main page and drained
// separately, in order, once a decoder that reads them is deployed.

// HoldPrefix is the consumer-name prefix of a hold row.
const HoldPrefix = "sched.hold."

func holdConsumer(m domain.MissionID) string { return HoldPrefix + string(m) }

// HeldStream is one hold row.
type HeldStream struct {
	Mission domain.MissionID
	// LastID is the last outbox id of this mission that was applied; the
	// drain resumes after it.
	LastID int64
	Since  time.Time
}

type querier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// Held lists the held missions, oldest first.
func (s *Store) Held(ctx context.Context) ([]HeldStream, error) {
	return heldList(ctx, s.pool)
}

func heldList(ctx context.Context, q querier) ([]HeldStream, error) {
	rows, err := q.Query(ctx, `SELECT substr(consumer, $1), last_id, updated_at FROM outbox_cursors
		WHERE consumer LIKE $2 ORDER BY updated_at, consumer LIMIT 1000`, len(HoldPrefix)+1, HoldPrefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HeldStream
	for rows.Next() {
		var h HeldStream
		if err := rows.Scan(&h.Mission, &h.LastID, &h.Since); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// HeldCount is the number of distinct held missions.
func (s *Store) HeldCount(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(DISTINCT consumer) FROM outbox_cursors WHERE consumer LIKE $1`, HoldPrefix+"%").Scan(&n)
	return n, err
}

func isHeld(ctx context.Context, q querier, m domain.MissionID) (bool, error) {
	var one int
	err := q.QueryRow(ctx, `SELECT 1 FROM outbox_cursors WHERE consumer = $1`, holdConsumer(m)).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// ApplyFunc applies one decoded outbox row inside the consumer's transaction.
type ApplyFunc func(ctx context.Context, tx pgx.Tx, r OutboxRow) error

// pageFrom reads outbox rows after `after`, up to `upto` when upto >= 0, for
// one mission when mission != "", with the same xid visibility filter the old
// page used. Each row carries its view or the decode error that holds it.
func pageFrom(ctx context.Context, q querier, after, upto int64, mission domain.MissionID, limit int) ([]OutboxRow, error) {
	rows, err := q.Query(ctx, `SELECT o.id, o.mission_id, o.seq, COALESCE(e.kind, ''), COALESCE(e.actor, ''), COALESCE(e.at, o.at), COALESCE(e.payload::text, '')
		FROM outbox o LEFT JOIN events e ON e.mission_id = o.mission_id AND e.seq = o.seq
		WHERE o.id > $1 AND ($2 < 0 OR o.id <= $2) AND ($3 = '' OR o.mission_id = $3)
		  AND o.xid < pg_snapshot_xmin(pg_current_snapshot())
		ORDER BY o.id LIMIT $4`, after, upto, string(mission), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboxRow
	for rows.Next() {
		var r OutboxRow
		er := envelope.Row{}
		var payload string
		if err := rows.Scan(&r.ID, &r.Mission, &r.Seq, &er.Kind, &er.Actor, &er.At, &payload); err != nil {
			return nil, err
		}
		er.Mission, er.Seq, er.Payload = string(r.Mission), r.Seq, []byte(payload)
		v, err := envelope.Upcast(er)
		if err != nil {
			r.Err = fmt.Errorf("outbox %d mission %s seq %d: %w", r.ID, r.Mission, r.Seq, err)
		} else {
			r.View, r.Event = v, v.Event
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func ensureCursor(ctx context.Context, tx pgx.Tx, consumer string) (int64, error) {
	if _, err := tx.Exec(ctx, `INSERT INTO outbox_cursors (consumer, last_id) VALUES ($1, 0) ON CONFLICT (consumer) DO NOTHING`, consumer); err != nil {
		return 0, err
	}
	var cur int64
	err := tx.QueryRow(ctx, `SELECT last_id FROM outbox_cursors WHERE consumer = $1 FOR UPDATE`, consumer).Scan(&cur)
	return cur, err
}

// ConsumeOutbox reads one page for the consumer, applies each row of a
// healthy mission, holds a mission whose row cannot be decoded, skips rows of
// held missions, and advances the cursor, all in one transaction that locks
// the cursor row (so a second scheduler cannot read the same page, and a crash
// between apply and ack can only redeliver). It returns the rows applied:
// skipped and newly held rows are not work.
func (s *Store) ConsumeOutbox(ctx context.Context, consumer string, limit int, apply ApplyFunc) (int, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck
	cur, err := ensureCursor(ctx, tx, consumer)
	if err != nil {
		return 0, err
	}
	page, err := pageFrom(ctx, tx, cur, -1, "", limit)
	if err != nil || len(page) == 0 {
		return 0, err
	}
	held := map[domain.MissionID]bool{}
	applied, maxID := 0, cur
	var newly []domain.MissionID
	for _, r := range page {
		if r.ID > maxID {
			maxID = r.ID
		}
		h, ok := held[r.Mission]
		if !ok {
			if h, err = isHeld(ctx, tx, r.Mission); err != nil {
				return 0, err
			}
			held[r.Mission] = h
		}
		if h {
			continue
		}
		if r.Err != nil {
			// Hold from this row: the drain starts at it.
			if _, err := tx.Exec(ctx, `INSERT INTO outbox_cursors (consumer, last_id) VALUES ($1, $2) ON CONFLICT (consumer) DO NOTHING`,
				holdConsumer(r.Mission), r.ID-1); err != nil {
				return 0, err
			}
			held[r.Mission] = true
			newly = append(newly, r.Mission)
			continue
		}
		if err := apply(ctx, tx, r); err != nil {
			return 0, err
		}
		applied++
	}
	if maxID > cur {
		if _, err := tx.Exec(ctx, `UPDATE outbox_cursors SET last_id = $2, updated_at = now() WHERE consumer = $1`, consumer, maxID); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	for _, m := range newly {
		fmt.Fprintf(os.Stderr, "fabd: held %s: an event this build cannot decode; the rest keep flowing\n", m) // once per hold, not per tick
	}
	return applied, nil
}

// DrainHolds advances every hold row: it applies the held mission's rows in
// order up to the scheduler cursor, stops at the first row that still cannot
// be decoded, and deletes the hold row once it has caught up with the cursor.
// The deletion happens in a transaction that locks the cursor row, so a row is
// neither delivered twice nor skipped. It returns the rows applied.
func (s *Store) DrainHolds(ctx context.Context, consumer string, limit int, apply ApplyFunc) (int, error) {
	holds, err := s.Held(ctx)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, h := range holds {
		n, err := s.drainOne(ctx, consumer, h.Mission, limit, apply)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

func (s *Store) drainOne(ctx context.Context, consumer string, m domain.MissionID, limit int, apply ApplyFunc) (int, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck
	schedLast, err := ensureCursor(ctx, tx, consumer)
	if err != nil {
		return 0, err
	}
	var last int64
	err = tx.QueryRow(ctx, `SELECT last_id FROM outbox_cursors WHERE consumer = $1 FOR UPDATE`, holdConsumer(m)).Scan(&last)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil // released by another scheduler
	}
	if err != nil {
		return 0, err
	}
	orig := last
	page, err := pageFrom(ctx, tx, last, schedLast, m, limit)
	if err != nil {
		return 0, err
	}
	applied := 0
	stuck := false
	for _, r := range page {
		if r.Err != nil {
			stuck = true
			break
		}
		if err := apply(ctx, tx, r); err != nil {
			return 0, err
		}
		applied++
		last = r.ID
	}
	released := false
	caughtUp := !stuck && len(page) < limit
	if caughtUp {
		// A hold found by replay or by a start sits behind the cursor, so
		// there may be nothing left to drain while the log still does not
		// decode. Release only when the whole log reads through the v2
		// reader; otherwise stay held at the cursor.
		if _, err := readViews(ctx, tx, m); err != nil {
			if !envelope.Undecodable(err) {
				return 0, err
			}
			caughtUp = false
			last = schedLast
		}
	}
	if !caughtUp && last == orig {
		// Nothing moved: leave the hold row (and its age) untouched.
	} else if !caughtUp {
		// Stay held at the last applied row (stuck), or resume after a full page.
		if _, err := tx.Exec(ctx, `UPDATE outbox_cursors SET last_id = $2, updated_at = now() WHERE consumer = $1`, holdConsumer(m), last); err != nil {
			return 0, err
		}
	} else {
		// Caught up with the scheduler cursor: release.
		if _, err := tx.Exec(ctx, `DELETE FROM outbox_cursors WHERE consumer = $1`, holdConsumer(m)); err != nil {
			return 0, err
		}
		released = true
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	if released {
		fmt.Fprintf(os.Stderr, "fabd: released %s: its held rows are delivered\n", m)
	}
	return applied, nil
}

// HoldMission records a hold found outside the consumer (replay, a start
// that finds an unreadable log) at the current scheduler cursor, so rows
// already applied are never redelivered. It is idempotent.
func (s *Store) HoldMission(ctx context.Context, q interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, m domain.MissionID) error {
	_, err := q.Exec(ctx, `INSERT INTO outbox_cursors (consumer, last_id)
		VALUES ($1, COALESCE((SELECT last_id FROM outbox_cursors WHERE consumer = 'sched'), 0))
		ON CONFLICT (consumer) DO NOTHING`, holdConsumer(m))
	return err
}

// errHeldText is the operator-facing reason class for a held mission, never
// payload text.
func errHeldText(err error) string {
	switch {
	case errors.Is(err, envelope.ErrUnknownKind):
		return "unknown-kind"
	case errors.Is(err, envelope.ErrRowMismatch):
		return "row-mismatch"
	case errors.Is(err, envelope.ErrPayload):
		return "payload-type"
	}
	return "other"
}
