package store

import (
	"context"
	"time"

	"fabric/internal/domain"
)

// M1Start is one attempt whose START carried the M1 flag (goodqa#15966
// F3), with what restart recovery needs to settle or hold it.
type M1Start struct {
	Mission domain.MissionID
	Attempt domain.AttemptID
	Fence   int64
	Started time.Time
	State   domain.AttemptState
	// Launched: an ATTEMPT_ACKED event exists and no prelaunch FINISH;
	// false means no runner byte ran (F2).
	Launched bool
}

// m1StartsSQL reads the flag back from the START command's canonical
// request, the bytes stored in the START transaction. The CASE guards the
// jsonb cast: only a request that holds the flag's exact bytes is parsed.
const m1StartsSQL = `SELECT a.mission_id, a.id, a.fence, a.started_at, a.state,
	EXISTS (SELECT 1 FROM events e WHERE e.mission_id = a.mission_id AND e.kind = 'ATTEMPT_ACKED' AND e.payload->>'attempt' = a.id)
	AND NOT EXISTS (SELECT 1 FROM events e WHERE e.mission_id = a.mission_id AND e.kind = 'ATTEMPT_FINISHED'
		AND e.payload->>'attempt' = a.id AND COALESCE((e.payload->>'prelaunch')::boolean, false))
	FROM commands c JOIN attempts a ON a.mission_id = c.mission_id AND c.command_id = 'start:' || a.id
	WHERE c.kind = 'START_ATTEMPT' AND c.status = 'ok' AND c.request_format = 'CANONICAL_V1'
		AND CASE WHEN position('"m1":true'::bytea IN c.canonical_request) > 0
			THEN (convert_from(c.canonical_request, 'UTF8')::jsonb->>'m1') = 'true'
			AND (convert_from(c.canonical_request, 'UTF8')::jsonb->>'attempt') = a.id
			ELSE false END`

// M1Starts lists the M1-flagged attempts of host h, or of mission m when m
// is set (h is then ignored). Read-only.
func (s *Store) M1Starts(ctx context.Context, h domain.HostID, m domain.MissionID) ([]M1Start, error) {
	q, arg := m1StartsSQL+` AND a.host = $1 ORDER BY a.started_at, a.id`, string(h)
	if m != "" {
		q, arg = m1StartsSQL+` AND a.mission_id = $1 ORDER BY a.started_at, a.id`, string(m)
	}
	rows, err := s.pool.Query(ctx, q, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []M1Start
	for rows.Next() {
		var r M1Start
		var state string
		if err := rows.Scan(&r.Mission, &r.Attempt, &r.Fence, &r.Started, &state, &r.Launched); err != nil {
			return nil, err
		}
		r.State = domain.AttemptState(state)
		out = append(out, r)
	}
	return out, rows.Err()
}

// OperatorHolds lists the missions with an AC-34 operator hold, in order.
// Read-only.
func (s *Store) OperatorHolds(ctx context.Context) ([]domain.MissionID, error) {
	rows, err := s.pool.Query(ctx, `SELECT substr(consumer, $1) FROM outbox_cursors WHERE consumer LIKE $2 ORDER BY consumer`,
		len(OpHoldPrefix)+1, OpHoldPrefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.MissionID
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		out = append(out, domain.MissionID(m))
	}
	return out, rows.Err()
}
