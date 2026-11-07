package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"fabric/internal/domain"
)

// Grant is the stored decision for one attempt's dispatch (0012).
type Grant struct {
	Attempt      domain.AttemptID
	Fence        int64
	Granted      bool
	Reason       string
	Reservations []string
	DecidedAt    time.Time
	Replayed     bool // an earlier call decided; this caller must not launch
}

// AuthorizeDispatch decides, once, whether attempt may start its runner. It
// locks the attempt, then its HELD reservations by id, reads the database
// clock after the locks, and checks fence, state RUNNING, lease, intent
// revision and that every resource in need is held and unexpired. The
// decision is stored; a later call returns it with Replayed set.
func (s *Store) AuthorizeDispatch(ctx context.Context, attempt domain.AttemptID, fence int64, need []string) (Grant, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Grant{}, err
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck
	var mission, state string
	var cur, revision int64
	var leaseUntil *time.Time
	err = tx.QueryRow(ctx, `SELECT mission_id, fence, revision, state, lease_until FROM attempts WHERE id = $1 FOR UPDATE`,
		string(attempt)).Scan(&mission, &cur, &revision, &state, &leaseUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return Grant{}, fmt.Errorf("%w: attempt %s", domain.ErrNotFound, attempt)
	}
	if err != nil {
		return Grant{}, err
	}
	if cur != fence {
		// A caller holding another fence is refused and nothing is stored.
		return Grant{}, fmt.Errorf("%w: attempt %s is at fence %d, not %d", domain.ErrStaleFence, attempt, cur, fence)
	}
	if g, ok, err := loadGrant(ctx, tx, attempt); err != nil || ok {
		g.Replayed = ok
		return g, err
	}
	type held struct {
		id, resource string
		expires      time.Time
	}
	rows, err := tx.Query(ctx, `SELECT id, resource, expires_at FROM reservations
		WHERE attempt_id = $1 AND fence = $2 AND state = 'HELD' ORDER BY id FOR UPDATE`, string(attempt), fence)
	if err != nil {
		return Grant{}, err
	}
	var hs []held
	for rows.Next() {
		var h held
		if err := rows.Scan(&h.id, &h.resource, &h.expires); err != nil {
			rows.Close()
			return Grant{}, err
		}
		hs = append(hs, h)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Grant{}, err
	}
	var intentRevision int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE((state->'version'->>'intent_revision')::bigint, 0) FROM missions WHERE id = $1`, mission).
		Scan(&intentRevision); err != nil {
		return Grant{}, err
	}
	if s.hooks.grantLocked != nil {
		s.hooks.grantLocked()
	}
	// DB time after every lock (F3): now() is the transaction start.
	var at time.Time
	if err := tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&at); err != nil {
		return Grant{}, err
	}
	g := Grant{Attempt: attempt, Fence: fence, DecidedAt: at.UTC(), Reservations: []string{}}
	byResource := map[string]held{}
	for _, h := range hs {
		byResource[h.resource] = h
	}
	g.Reason = staleReason(state, leaseUntil, at, revision, intentRevision)
	if g.Reason == "" {
		ids := []string{}
		for _, r := range need {
			h, ok := byResource[r]
			if !ok {
				g.Reason = "no held reservation for " + r
				break
			}
			if !h.expires.After(at) {
				g.Reason = "reservation for " + r + " expired"
				break
			}
			ids = append(ids, h.id)
		}
		if g.Reason == "" && len(ids) == 0 {
			g.Reason = "no resources required" // a grant always names what it consumed
		}
		if g.Reason == "" {
			sort.Strings(ids)
			g.Granted, g.Reservations = true, ids
		}
	}
	outcome := "DENIED"
	if g.Granted {
		outcome = "GRANTED"
	}
	if _, err := tx.Exec(ctx, `INSERT INTO dispatch_grants (attempt_id, fence, outcome, reason, reservations, decided_at)
		VALUES ($1, $2, $3, $4, $5, $6)`, string(attempt), fence, outcome, g.Reason, g.Reservations, at); err != nil {
		return Grant{}, err
	}
	return g, tx.Commit(ctx)
}

// staleReason is the admission check shared by the gated grant and the
// ungated freshness check: "" means the attempt may still start.
func staleReason(state string, leaseUntil *time.Time, at time.Time, revision, intentRevision int64) string {
	switch {
	case domain.AttemptState(state) != domain.AttemptRunning:
		return fmt.Sprintf("attempt is %s, not RUNNING", state)
	case leaseUntil == nil || !leaseUntil.After(at):
		return "attempt lease expired"
	case revision != intentRevision:
		return fmt.Sprintf("intent revised: attempt at revision %d, mission at %d", revision, intentRevision)
	}
	return ""
}

// CheckDispatchFresh is the final admission when the dispatch gate is off
// (goodqa#15082 T7): the same fence, RUNNING, lease and intent-revision
// checks as AuthorizeDispatch, against the database clock, with no
// reservations and nothing stored. A non-empty reason is a definite denial.
func (s *Store) CheckDispatchFresh(ctx context.Context, attempt domain.AttemptID, fence int64) (string, error) {
	var state string
	var cur, revision, intentRevision int64
	var leaseUntil *time.Time
	var at time.Time
	err := s.pool.QueryRow(ctx, `SELECT a.fence, a.revision, a.state, a.lease_until,
			COALESCE((m.state->'version'->>'intent_revision')::bigint, 0), clock_timestamp()
		FROM attempts a JOIN missions m ON m.id = a.mission_id WHERE a.id = $1`,
		string(attempt)).Scan(&cur, &revision, &state, &leaseUntil, &intentRevision, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: attempt %s", domain.ErrNotFound, attempt)
	}
	if err != nil {
		return "", err
	}
	if cur != fence {
		return "", fmt.Errorf("%w: attempt %s is at fence %d, not %d", domain.ErrStaleFence, attempt, cur, fence)
	}
	return staleReason(state, leaseUntil, at, revision, intentRevision), nil
}

func loadGrant(ctx context.Context, q pgx.Tx, attempt domain.AttemptID) (Grant, bool, error) {
	g := Grant{Attempt: attempt}
	var outcome string
	err := q.QueryRow(ctx, `SELECT fence, outcome, reason, reservations, decided_at FROM dispatch_grants WHERE attempt_id = $1`,
		string(attempt)).Scan(&g.Fence, &outcome, &g.Reason, &g.Reservations, &g.DecidedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Grant{}, false, nil
	}
	if err != nil {
		return Grant{}, false, err
	}
	g.Granted = outcome == "GRANTED"
	g.DecidedAt = g.DecidedAt.UTC()
	return g, true, nil
}
