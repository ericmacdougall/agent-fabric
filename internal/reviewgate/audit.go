package reviewgate

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// AuditEntry contains metadata only; there is no proposal, task or evidence
// payload field to populate accidentally.
type AuditEntry struct {
	Seq           int64     `json:"seq"`
	RequestHash   string    `json:"request_hash"`
	Event         string    `json:"event"`
	Actor         string    `json:"actor"`
	Detail        string    `json:"detail"`
	PolicyVersion string    `json:"policy_version"`
	At            time.Time `json:"-"`
	PrevHash      string    `json:"prev_hash"`
	Hash          string    `json:"-"`
}

func auditDigest(e AuditEntry) (string, error) {
	type digest struct {
		Seq           int64  `json:"seq"`
		RequestHash   string `json:"request_hash"`
		Event         string `json:"event"`
		Actor         string `json:"actor"`
		Detail        string `json:"detail"`
		PolicyVersion string `json:"policy_version"`
		At            string `json:"at"`
		PrevHash      string `json:"prev_hash"`
	}
	b, err := json.Marshal(digest{e.Seq, e.RequestHash, e.Event, e.Actor, e.Detail,
		e.PolicyVersion, e.At.UTC().Format(time.RFC3339Nano), e.PrevHash})
	if err != nil {
		return "", err
	}
	return hashBytes(b), nil
}

func appendAudit(ctx context.Context, tx pgx.Tx, requestHash, event, actor, detail, policyVersion string) error {
	if !validText(actor, MaxNameBytes) || !validText(event, 128) {
		return invalid("invalid audit actor or event")
	}
	if len(detail) > MaxAuditText {
		detail = boundedReason(detail)
	}
	var seq int64
	var prev string
	var at time.Time
	if err := tx.QueryRow(ctx, `SELECT seq, last_hash, clock_timestamp() FROM review_gate_audit_head WHERE id = 1 FOR UPDATE`).Scan(&seq, &prev, &at); err != nil {
		return err
	}
	e := AuditEntry{Seq: seq + 1, RequestHash: requestHash, Event: event, Actor: actor,
		Detail: detail, PolicyVersion: policyVersion, At: at, PrevHash: prev}
	hash, err := auditDigest(e)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO review_gate_audit
		(seq, request_hash, event, actor, detail, policy_version, at, prev_hash, hash)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, e.Seq, e.RequestHash, e.Event, e.Actor,
		e.Detail, e.PolicyVersion, e.At, e.PrevHash, hash); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE review_gate_audit_head SET seq = $2, last_hash = $3 WHERE id = 1 AND seq = $1`, seq, e.Seq, hash)
	return err
}

// VerifyAudit recomputes the complete chain and requires gap-free sequence and
// parent links. Database owners can still rewrite an entire chain; this detects
// corruption and unauthorized ordinary writers, not a malicious superuser.
func (s *PostgresStore) VerifyAudit(ctx context.Context) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck
	rows, err := tx.Query(ctx, `SELECT seq, request_hash, event, actor, detail, policy_version, at, prev_hash, hash
		FROM review_gate_audit ORDER BY seq`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var want int64 = 1
	prev := ""
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.Seq, &e.RequestHash, &e.Event, &e.Actor, &e.Detail,
			&e.PolicyVersion, &e.At, &e.PrevHash, &e.Hash); err != nil {
			return err
		}
		got, err := auditDigest(e)
		if err != nil {
			return err
		}
		if e.Seq != want || e.PrevHash != prev || e.Hash != got {
			return fmt.Errorf("audit chain mismatch at sequence %d", e.Seq)
		}
		prev = e.Hash
		want++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	var headSeq int64
	var headHash string
	if err := tx.QueryRow(ctx, `SELECT seq, last_hash FROM review_gate_audit_head WHERE id = 1`).Scan(&headSeq, &headHash); err != nil {
		return err
	}
	if headSeq != want-1 || headHash != prev {
		return fmt.Errorf("audit head does not match the chain")
	}
	return tx.Commit(ctx)
}

// Audit returns at most the first 1000 entries. Use AuditPage for complete,
// bounded incremental reads; VerifyAudit streams the chain without buffering.
func (s *PostgresStore) Audit(ctx context.Context) ([]AuditEntry, error) {
	return s.AuditPage(ctx, 0, 1000)
}

func (s *PostgresStore) AuditPage(ctx context.Context, afterSeq int64, limit int) ([]AuditEntry, error) {
	if afterSeq < 0 || limit < 1 || limit > 1000 {
		return nil, invalid("audit cursor or page limit outside [1, 1000]")
	}
	rows, err := s.pool.Query(ctx, `SELECT seq, request_hash, event, actor, detail, policy_version, at, prev_hash, hash
		FROM review_gate_audit WHERE seq > $1 ORDER BY seq LIMIT $2`, afterSeq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AuditEntry, 0, limit)
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.Seq, &e.RequestHash, &e.Event, &e.Actor, &e.Detail,
			&e.PolicyVersion, &e.At, &e.PrevHash, &e.Hash); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
