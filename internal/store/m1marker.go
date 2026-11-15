package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// CommentPolicyMarker reads the fabric_settings comment_policy row
// (goodqa#15966): its value, and whether it exists. Read-only.
func (s *Store) CommentPolicyMarker(ctx context.Context) (string, bool, error) {
	var v string
	err := s.pool.QueryRow(ctx, `SELECT value FROM fabric_settings WHERE key = 'comment_policy'`).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}
