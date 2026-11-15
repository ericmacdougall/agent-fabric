package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"fabric/internal/domain"
)

// An operator hold (goodqa#15966 owner ruling R1) keeps a mission from any
// new attempt until an operator releases it: after an M1 settle failed
// closed (credential_material_unavailable, credential_rotated_mid_attempt,
// redaction_numeric_hit, redaction_broke_stream) and after a refused
// subscription token (AC-19). It is a row in the existing outbox_cursors
// table under its own consumer prefix, so it is data, not a migration
// (I-4). Unlike HoldMission's undecodable-log hold ("sched.hold."), no drain
// ever releases it, and it does not exempt a leased attempt from lease
// expiry or reconcile: only dispatch selection and the pre-START check read
// it. The reason class and the attempt are kept in the AC-29 records, never
// here.

// OpHoldPrefix is the consumer-name prefix of an operator hold row.
const OpHoldPrefix = "sched.ophold."

func opHoldConsumer(m domain.MissionID) string { return OpHoldPrefix + string(m) }

// OperatorHold holds m. It is idempotent: a second hold keeps the first
// row's time.
func (s *Store) OperatorHold(ctx context.Context, m domain.MissionID) error {
	if _, err := domain.ParseMissionID(string(m)); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO outbox_cursors (consumer, last_id) VALUES ($1, 0) ON CONFLICT (consumer) DO NOTHING`, opHoldConsumer(m))
	return err
}

// OperatorHeld reports whether m has an operator hold.
func (s *Store) OperatorHeld(ctx context.Context, m domain.MissionID) (bool, error) {
	var one int
	err := s.pool.QueryRow(ctx, `SELECT 1 FROM outbox_cursors WHERE consumer = $1`, opHoldConsumer(m)).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// ReleaseOperatorHold removes m's operator hold and reports whether one
// existed.
func (s *Store) ReleaseOperatorHold(ctx context.Context, m domain.MissionID) (bool, error) {
	if _, err := domain.ParseMissionID(string(m)); err != nil {
		return false, err
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM outbox_cursors WHERE consumer = $1`, opHoldConsumer(m))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
