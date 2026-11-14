package effects

import (
	"context"
	"time"

	"fabric/internal/domain"
	"fabric/internal/store"
)

// TimelineRow is one line of `fab events --mission M` (goodqa#15966 AC-12):
// a mission event, or an effect state read from the effects table at read
// time. No event kind is added: effect rows are a read-time join.
type TimelineRow struct {
	At      time.Time `json:"at"`
	Label   string    `json:"label"` // event kind, or effect:{state}
	Seq     int64     `json:"seq,omitempty"`
	Attempt string    `json:"attempt,omitempty"`
	Effect  string    `json:"effect,omitempty"`
	Key     string    `json:"key,omitempty"`
}

// Timeline merges a mission's events with its effects, ordered by time:
// each effect contributes its submit (effect:INTENT or
// effect:APPROVAL_REQUIRED), each recorded dispatch (effect:DISPATCHED) and
// its current state at its last update (effect:{state}).
func Timeline(ctx context.Context, st *store.Store, mission domain.MissionID) ([]TimelineRow, error) {
	rows, err := st.Pool().Query(ctx, `
		SELECT at, kind, seq, COALESCE(payload->>'attempt', ''), '', '' FROM events WHERE mission_id = $1
		UNION ALL
		SELECT created_at, 'effect:' || CASE WHEN approval_sha IS NOT NULL OR state = 'APPROVAL_REQUIRED' THEN 'APPROVAL_REQUIRED' ELSE 'INTENT' END,
			0, attempt_id, id, idem_key FROM effects WHERE mission_id = $1
		UNION ALL
		SELECT d.at, 'effect:DISPATCHED', 0, e.attempt_id, e.id, e.idem_key FROM effect_dispatches d JOIN effects e ON e.id = d.effect_id WHERE e.mission_id = $1
		UNION ALL
		SELECT updated_at, 'effect:' || state, 0, attempt_id, id, idem_key FROM effects
			WHERE mission_id = $1 AND state NOT IN ('INTENT', 'APPROVAL_REQUIRED', 'DISPATCHED')
		ORDER BY 1, 3`, string(mission))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TimelineRow
	for rows.Next() {
		var r TimelineRow
		if err := rows.Scan(&r.At, &r.Label, &r.Seq, &r.Attempt, &r.Effect, &r.Key); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
