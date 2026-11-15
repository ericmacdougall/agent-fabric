package store

import (
	"context"
	"crypto/rand"
	"encoding/binary"
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

// OperatorHold holds m under a fresh random generation (spec v16; the
// generation is last_id). A re-hold keeps the existing generation, and its
// no-op DO UPDATE takes the row lock, so it waits for an in-flight release
// that holds the row FOR UPDATE: if that release deletes the row first,
// this insert creates a new row with the new generation, which the
// release's authorization does not name (red-team v16 p1). DO NOTHING would
// not wait and would fold into the generation about to be deleted.
//
// Lock order (v16 p2): the database row lock is always taken before the
// records file lock, here (fabd writes mission_hold after this commits)
// and in the release.
func (s *Store) OperatorHold(ctx context.Context, m domain.MissionID) error {
	if _, err := domain.ParseMissionID(string(m)); err != nil {
		return err
	}
	gen, err := newGeneration()
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO outbox_cursors (consumer, last_id) VALUES ($1, $2)
		ON CONFLICT (consumer) DO UPDATE SET last_id = outbox_cursors.last_id`, opHoldConsumer(m), gen)
	return err
}

func newGeneration() (int64, error) {
	var b [8]byte
	for {
		if _, err := rand.Read(b[:]); err != nil {
			return 0, err
		}
		if g := int64(binary.BigEndian.Uint64(b[:]) >> 1); g > 0 {
			return g, nil
		}
	}
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

// ReleaseOperatorHold releases m's hold in one transaction (spec v16): it
// locks the row FOR UPDATE and reads its generation G, calls authorize(G),
// which must write the release_authorized record durably, and only then
// deletes the row where the generation is still G and commits. An
// authorize error rolls back, so the hold stays. It reports whether a hold
// existed and was released.
func (s *Store) ReleaseOperatorHold(ctx context.Context, m domain.MissionID, authorize func(gen int64) error) (bool, error) {
	if _, err := domain.ParseMissionID(string(m)); err != nil {
		return false, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck
	var gen int64
	err = tx.QueryRow(ctx, `SELECT last_id FROM outbox_cursors WHERE consumer = $1 FOR UPDATE`, opHoldConsumer(m)).Scan(&gen)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := authorize(gen); err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, `DELETE FROM outbox_cursors WHERE consumer = $1 AND last_id = $2`, opHoldConsumer(m), gen)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() != 1 {
		return false, errors.New("operator hold changed under its row lock")
	}
	return true, tx.Commit(ctx)
}
