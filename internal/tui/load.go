package tui

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"fabric/internal/metrics"
)

// rowLimit bounds every pane query; textLimit bounds every free-text column
// (in characters) before it leaves the database.
const (
	rowLimit  = 500
	textLimit = 512
)

// Header holds the /metrics gauges shown on the first lines.
type Header struct {
	Version                           string
	Schema                            int64
	MissionsActive, Missions          int64
	AttemptsLive, AttemptsStale       int64
	Unknown, Quarantined, Reconciling int64
	Inflight, Overdue, Limit          int64
	AsksOpen                          int64
	SpentMonth, MonthCap              int64 // USD micros
}

// Mission is one row of the missions pane.
type Mission struct {
	ID, Ticket, Execution, Terminal string
	Created                         time.Time
	Spent                           int64 // USD micros over its attempts
}

// Attempt is one active attempt.
type Attempt struct {
	ID, Mission, State string
	Fence              int64
	Lease              *time.Time
}

// Effect is one effect; the panes show those needing attention.
type Effect struct {
	ID, Mission, Kind, State string
	Updated                  time.Time
	Dispatches               int64
}

// Ask is one ask; the pane shows open ones.
type Ask struct {
	ID, Mission, Kind, State, Question string
	Created                            time.Time
}

// Resolution is a recorded operator decision on an effect.
type Resolution struct {
	Effect, From, To, Actor string
	At                      time.Time
}

// Data is one refresh, read in one transaction. Now is the database's
// clock at that transaction, so ages do not depend on the local clock.
type Data struct {
	Now      time.Time
	Header   Header
	Missions []Mission
	Attempts []Attempt
	Effects  []Effect
	Asks     []Ask
}

// Detail is the drill-down for one mission.
type Detail struct {
	Mission     Mission
	Found       bool
	Attempts    []Attempt
	Effects     []Effect
	Asks        []Ask
	Resolutions []Resolution
}

// Source loads the panes (and a mission's detail when detailID is set).
type Source struct {
	Pool    *pgxpool.Pool
	Timeout time.Duration   // per refresh; metrics.DefaultTimeout when 0
	Metrics metrics.Options // version, dispatch limit and caps for the header
}

// OpenPool opens a lazy pool for the TUI. Every session it opens is
// read-only by default (default_transaction_read_only), on top of the
// read-only transaction metrics.ReadOnly opens, so no TUI query can write.
func OpenPool(dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("FABRIC_DSN does not parse") // the error may quote the DSN
	}
	cfg.MaxConns = 2
	cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	cfg.ConnConfig.RuntimeParams["application_name"] = "fab-tui"
	return pgxpool.NewWithConfig(context.Background(), cfg)
}

// Fetch reads one refresh through metrics.ReadOnly.
func (s *Source) Fetch(ctx context.Context, detailID string) (*Data, *Detail, error) {
	var d *Data
	var det *Detail
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = metrics.DefaultTimeout
	}
	err := metrics.ReadOnly(ctx, s.Pool, timeout, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if d, err = loadData(ctx, tx, s.Metrics); err != nil {
			return err
		}
		if detailID != "" {
			det, err = loadDetail(ctx, tx, detailID)
		}
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return d, det, nil
}

func loadData(ctx context.Context, tx pgx.Tx, o metrics.Options) (*Data, error) {
	d := &Data{}
	if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&d.Now); err != nil {
		return nil, err
	}
	snap, err := metrics.CollectTx(ctx, tx, o)
	if err != nil {
		return nil, err
	}
	d.Header = header(snap, o)
	if d.Missions, err = missions(ctx, tx, `SELECT m.id, left(m.ticket, 512), m.execution, m.terminal, m.created_at,
		COALESCE((SELECT sum(am.cost_micros) FROM agent_messages am JOIN attempts a ON a.id = am.attempt_id
		           WHERE a.mission_id = m.id), 0)::bigint
		  FROM missions m ORDER BY (m.terminal = 'NONE') DESC,
		   array_position(ARRAY['RUNNING', 'ADMITTED', 'WAITING', 'RESOURCE_EXHAUSTED', 'BLOCKED', 'PAUSED', 'IDLE'], m.execution),
		   m.created_at, m.id LIMIT 500`); err != nil {
		return nil, err
	}
	if d.Attempts, err = attempts(ctx, tx, `SELECT id, mission_id, state, fence, lease_until FROM attempts
		WHERE state IN ('LEASED', 'PREPARING', 'RUNNING') ORDER BY lease_until NULLS FIRST, id LIMIT 500`); err != nil {
		return nil, err
	}
	if d.Effects, err = effects(ctx, tx, `SELECT id, mission_id, kind, state, updated_at, dispatches FROM effects
		WHERE state IN ('UNKNOWN', 'QUARANTINED', 'RECONCILING') ORDER BY updated_at, id LIMIT 500`); err != nil {
		return nil, err
	}
	if d.Asks, err = asks(ctx, tx, `SELECT id, COALESCE(mission_id, ''), kind, state, left(question, 512), created_at
		FROM asks WHERE state = 'open' ORDER BY created_at, id LIMIT 500`); err != nil {
		return nil, err
	}
	return d, nil
}

func loadDetail(ctx context.Context, tx pgx.Tx, id string) (*Detail, error) {
	det := &Detail{}
	ms, err := missions(ctx, tx, `SELECT m.id, left(m.ticket, 512), m.execution, m.terminal, m.created_at,
		COALESCE((SELECT sum(am.cost_micros) FROM agent_messages am JOIN attempts a ON a.id = am.attempt_id
		           WHERE a.mission_id = m.id), 0)::bigint
		  FROM missions m WHERE m.id = $1`, id)
	if err != nil || len(ms) == 0 {
		return det, err
	}
	det.Mission, det.Found = ms[0], true
	if det.Attempts, err = attempts(ctx, tx, `SELECT id, mission_id, state, fence, lease_until FROM attempts
		WHERE mission_id = $1 ORDER BY fence, id LIMIT 500`, id); err != nil {
		return nil, err
	}
	if det.Effects, err = effects(ctx, tx, `SELECT id, mission_id, kind, state, updated_at, dispatches FROM effects
		WHERE mission_id = $1 ORDER BY created_at, id LIMIT 500`, id); err != nil {
		return nil, err
	}
	if det.Asks, err = asks(ctx, tx, `SELECT id, COALESCE(mission_id, ''), kind, state, left(question, 512), created_at
		FROM asks WHERE mission_id = $1 ORDER BY created_at, id LIMIT 500`, id); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT r.effect_id, r.from_state, r.to_state, left(r.actor, 512), r.at
		FROM effect_resolutions r JOIN effects e ON e.id = r.effect_id
		WHERE e.mission_id = $1 ORDER BY r.at, r.id LIMIT 500`, id)
	if err != nil {
		return nil, err
	}
	det.Resolutions, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Resolution, error) {
		var v Resolution
		return v, r.Scan(&v.Effect, &v.From, &v.To, &v.Actor, &v.At)
	})
	return det, err
}

func missions(ctx context.Context, tx pgx.Tx, q string, args ...any) ([]Mission, error) {
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Mission, error) {
		var v Mission
		return v, r.Scan(&v.ID, &v.Ticket, &v.Execution, &v.Terminal, &v.Created, &v.Spent)
	})
}

func attempts(ctx context.Context, tx pgx.Tx, q string, args ...any) ([]Attempt, error) {
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Attempt, error) {
		var v Attempt
		return v, r.Scan(&v.ID, &v.Mission, &v.State, &v.Fence, &v.Lease)
	})
}

func effects(ctx context.Context, tx pgx.Tx, q string, args ...any) ([]Effect, error) {
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Effect, error) {
		var v Effect
		return v, r.Scan(&v.ID, &v.Mission, &v.Kind, &v.State, &v.Updated, &v.Dispatches)
	})
}

func asks(ctx context.Context, tx pgx.Tx, q string, args ...any) ([]Ask, error) {
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Ask, error) {
		var v Ask
		return v, r.Scan(&v.ID, &v.Mission, &v.Kind, &v.State, &v.Question, &v.Created)
	})
}

func header(s *metrics.Snapshot, o metrics.Options) Header {
	n := func(name string, m ...[2]string) int64 { return int64(s.Sum(name, m...)) }
	h := Header{
		Version:        metrics.Label(o.Version),
		Schema:         n("fabric_schema_level"),
		Missions:       n("fabric_missions"),
		MissionsActive: n("fabric_missions", [2]string{"terminal", "NONE"}),
		AttemptsLive:   n("fabric_attempts_active", [2]string{"lease", "live"}),
		AttemptsStale:  n("fabric_attempts_active", [2]string{"lease", "stale"}),
		Unknown:        n("fabric_effects", [2]string{"state", "UNKNOWN"}),
		Quarantined:    n("fabric_effects", [2]string{"state", "QUARANTINED"}),
		Reconciling:    n("fabric_effects", [2]string{"state", "RECONCILING"}),
		Inflight:       n("fabric_dispatch_inflight"),
		Overdue:        n("fabric_dispatch_overdue"),
		Limit:          int64(o.DispatchLimit),
		AsksOpen:       n("fabric_asks_open"),
		SpentMonth:     n("fabric_native_spent_month_usd_micros"),
		MonthCap:       n("fabric_budget_cap_usd_micros", [2]string{"scope", "month"}),
	}
	return h
}

// Failure is how a failed refresh is shown: never the error text, which can
// quote the DSN, host or user; only its class and SQLSTATE.
type Failure struct {
	Disconnected bool   // the database is unreachable or refused the session
	Code         string // SQLSTATE when the server answered
}

// classify maps a refresh error to a Failure. A server error outside the
// connection (08) and operator-intervention (57P) classes means the
// database answered but this refresh failed (a lock or statement timeout):
// the data is stale, the connection is not lost.
func classify(err error) Failure {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		code := pg.Code
		lost := len(code) >= 2 && code[:2] == "08" || len(code) >= 3 && code[:3] == "57P" || code == "55000" || code == "3D000"
		var ce *pgconn.ConnectError
		if errors.As(err, &ce) {
			lost = true
		}
		return Failure{Disconnected: lost, Code: code}
	}
	// The client deadline (timeout plus one second) passed: the server did
	// not answer in time, which is stale data, not a lost connection (review
	// F2 on ff48df1).
	if errors.Is(err, context.DeadlineExceeded) {
		return Failure{}
	}
	return Failure{Disconnected: true}
}

func (f Failure) String() string {
	if f.Code != "" {
		return fmt.Sprintf("SQLSTATE %s", f.Code)
	}
	return "no answer"
}
