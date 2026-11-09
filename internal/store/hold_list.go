package store

import (
	"context"
	"regexp"
	"time"

	"fabric/internal/domain"
	"fabric/internal/envelope"
)

// HeldInfo is what `fab streams held` prints for one mission: the event kind
// of the first row that cannot be read (a short name, never payload text), a
// reason class and when the hold began.
type HeldInfo struct {
	Mission domain.MissionID
	Kind    string
	Reason  string
	Since   time.Time
	Held    bool // false: found by a scan, not yet held by the consumer
}

var kindShape = regexp.MustCompile(`^[A-Z0-9_]{1,64}$`)

// safeKind returns kind when it has the shape of an event kind name, and
// "invalid" otherwise, so no stored text is echoed.
func safeKind(k string) string {
	if kindShape.MatchString(k) {
		return k
	}
	return "invalid"
}

// HeldInfos lists held missions without decoding any payload: the first
// outbox row after the hold's last applied id names the kind, and the reason
// class is "unknown-kind" for a kind this build lacks and "undecodable"
// otherwise. A full decode is ScanUndecodable (`--scan`).
func (s *Store) HeldInfos(ctx context.Context, after string, limit int) ([]HeldInfo, error) {
	rows, err := s.pool.Query(ctx, `SELECT substr(c.consumer, $1), c.updated_at,
			COALESCE((SELECT e.kind FROM outbox o JOIN events e ON e.mission_id = o.mission_id AND e.seq = o.seq
			          WHERE o.mission_id = substr(c.consumer, $1) AND o.id > c.last_id ORDER BY o.id LIMIT 1), '')
		FROM outbox_cursors c WHERE c.consumer LIKE $2 AND substr(c.consumer, $1) > $3
		ORDER BY substr(c.consumer, $1) LIMIT $4`, len(HoldPrefix)+1, HoldPrefix+"%", after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HeldInfo
	for rows.Next() {
		var h HeldInfo
		var kind string
		if err := rows.Scan(&h.Mission, &h.Since, &kind); err != nil {
			return nil, err
		}
		h.Held = true
		h.Kind = safeKind(kind)
		switch {
		case kind == "":
			h.Kind, h.Reason = "unknown", "undecodable"
		case !domain.KnownEventKind(domain.EventKind(kind)):
			h.Reason = "unknown-kind"
		default:
			h.Reason = "undecodable"
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ScanUndecodable decodes events of missions after `after` (a mission id,
// keyset paged) and returns the missions whose log holds a row this build
// cannot decode, with the first failing kind and a reason class. It is the
// only full decode of history, used by `fab streams held --scan` and the
// rehearsal; it reads, never writes.
func (s *Store) ScanUndecodable(ctx context.Context, after string, limit int) ([]HeldInfo, string, error) {
	var upper string
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE(max(id), '') FROM (SELECT id FROM missions WHERE id > $1 ORDER BY id LIMIT $2) x`, after, limit).Scan(&upper); err != nil {
		return nil, after, err
	}
	if upper == "" {
		return nil, "", nil // no more missions
	}
	rows, err := s.pool.Query(ctx, `SELECT mission_id, seq, kind, actor, at, payload::text FROM events
		WHERE mission_id > $1 AND mission_id <= $2 ORDER BY mission_id, seq`, after, upper)
	if err != nil {
		return nil, after, err
	}
	defer rows.Close()
	var out []HeldInfo
	seen := map[domain.MissionID]bool{}
	for rows.Next() {
		var m domain.MissionID
		var r envelope.Row
		var payload string
		if err := rows.Scan(&m, &r.Seq, &r.Kind, &r.Actor, &r.At, &payload); err != nil {
			return nil, after, err
		}
		r.Mission, r.Payload = string(m), []byte(payload)
		if seen[m] {
			continue
		}
		if _, err := envelope.Upcast(r); err != nil {
			seen[m] = true
			reason := errHeldText(err)
			out = append(out, HeldInfo{Mission: m, Kind: safeKind(r.Kind), Reason: reason})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, after, err
	}
	held := map[domain.MissionID]bool{}
	if hs, err := s.Held(ctx); err == nil {
		for _, h := range hs {
			held[h.Mission] = true
		}
	}
	for i := range out {
		out[i].Held = held[out[i].Mission]
	}
	return out, upper, nil
}
