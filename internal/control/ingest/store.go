package ingest

import (
	"context"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// NotifyChannel carries a content-free hint after new rows are committed.
const NotifyChannel = "control_events"

// DB is what the store needs: a transaction and one statement after it.
type DB interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Stored says what one Store call did.
type Stored struct {
	Inserted int // new rows
	Existing int // ids already present, left untouched
}

const insertSQL = `INSERT INTO control.ingested_events
	(id, ts, kind, source, host, actor, initiative, repo, branch, ref, ticket, summary, severity, count, tenant)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
	ON CONFLICT DO NOTHING` // no conflict target: a target needs SELECT, which the ingest role does not have

// Column limits of control.ingested_events (migration 0039) that are tighter
// than the mc.event/1 schema. A value over its limit is cut (never refused), so
// one long field cannot abort a whole batch with a check violation.
const (
	maxActor      = 128
	maxInitiative = 128
	maxTicket     = 64
	maxCount      = 1_000_000
)

func cut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// args maps a validated event to the insert's parameters. actor is the first of
// agent, session and runtime that the producer set.
func args(e Event) []any {
	actor := ""
	for _, k := range []string{"agent", "session", "runtime"} {
		if v := e.Actor[k]; v != "" {
			actor = v
			break
		}
	}
	count := e.Count
	if count > maxCount {
		count = maxCount
	}
	var tenant any
	if e.Tenant != "" {
		tenant = cut(e.Tenant, 64)
	}
	return []any{e.ID, e.TS, e.Kind, e.Source, cut(e.Host, 128), cut(actor, maxActor), cut(e.Subject["initiative"], maxInitiative),
		cut(e.Subject["repo"], 200), cut(e.Subject["branch"], 200), cut(e.Subject["ref"], 200), cut(e.Subject["ticket"], maxTicket),
		e.Summary, e.Severity, count, tenant}
}

// Store writes the accepted events of a batch to control.ingested_events, all in
// one transaction (a database error leaves nothing behind), idempotent by id. It
// writes no other table: not fabric events, not the outbox. After the commit,
// and only when a row was inserted, it sends an empty NOTIFY control_events.
func Store(ctx context.Context, db DB, events []Event) (Stored, error) {
	var s Stored
	if len(events) == 0 {
		return s, nil
	}
	// Insert in id order: two concurrent calls with overlapping ids then take their
	// row locks in the same order and cannot deadlock (review finding on PR 49).
	events = append([]Event(nil), events...)
	sort.Slice(events, func(i, j int) bool { return events[i].ID < events[j].ID })
	tx, err := db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return s, err
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck // no-op after commit
	for _, e := range events {
		tag, err := tx.Exec(ctx, insertSQL, args(e)...)
		if err != nil {
			return Stored{}, fmt.Errorf("insert %s: %w", e.ID, err)
		}
		if tag.RowsAffected() == 1 {
			s.Inserted++
		} else {
			s.Existing++
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Stored{}, err
	}
	if s.Inserted > 0 {
		// A hint only: a failed notify is not an error, readers catch up from their cursor.
		nctx, cancel := context.WithTimeout(context.Background(), 2*1e9)
		defer cancel()
		db.Exec(nctx, "SELECT pg_notify($1, '')", NotifyChannel) //nolint:errcheck
	}
	return s, nil
}
