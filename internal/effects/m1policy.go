package effects

// The M1 comment policy (spec goodqa#15966 v12, "M1 comment policy"; AC-7,
// AC-32). Every broker that shares a database applies it from database facts
// only: the fabric_settings ('comment_policy', 'm1') marker row and the
// effect row it has locked in the deciding transaction. Nothing here reads
// mission config or matches a key prefix.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"fabric/internal/domain"
)

// GovernedKind is the effect kind the policy governs.
const GovernedKind = "ticket_comment"

// PolicyMarkerKey and PolicyMarkerM1 are the fabric_settings marker row.
const (
	PolicyMarkerKey = "comment_policy"
	PolicyMarkerM1  = "m1"
)

// Refusal codes.
const (
	CodeNonCanonical  = "m1_noncanonical_key"
	CodeSecondEffect  = "m1_second_effect"
	CodeNoRedispatch  = "m1_no_redispatch"
	CodeMarkerMissing = "m1_policy_marker_missing"
	CodePolicyUnknown = "comment_policy_unknown"
)

// PolicyRefusal is a refusal by the M1 comment policy: nothing changed. The
// CLI maps it to exit 3.
type PolicyRefusal struct {
	Code   string
	Detail string
}

func (r *PolicyRefusal) Error() string { return r.Code + ": " + r.Detail }

// IsPolicyRefusal reports whether err is a policy refusal with code (any code
// when code is "").
func IsPolicyRefusal(err error, code string) bool {
	var r *PolicyRefusal
	return errors.As(err, &r) && (code == "" || r.Code == code)
}

// M1Key is the single comment identity of a mission: "m1-" and the first 24
// hex of sha256(mission id). The scheduler and the broker share this one
// function; the key fits the GoodQA trailer grammar for any mission id.
func M1Key(m domain.MissionID) string {
	sum := sha256.Sum256([]byte(m))
	return "m1-" + hex.EncodeToString(sum[:])[:24]
}

// goodqaCommentServerTimeout is GoodQA's server-side request timeout for
// comment_on_issue. ASSUMPTION (owner, 2026-11-14): 60 s; GoodQA's source
// was not available to cite. goodqa#16042 item 4 makes citing the real
// value from GoodQA's configuration a mandatory PR-stage item.
const goodqaCommentServerTimeout = 60 * time.Second

// quietFloor is the minimum quiet window (AC-7).
const quietFloor = 15 * time.Minute

// QuietWindow is the AC-7 early-readback quiet window: the larger of 15
// minutes and twice the server timeout. The timeout can only widen it.
func QuietWindow() time.Duration {
	if w := 2 * goodqaCommentServerTimeout; w > quietFloor {
		return w
	}
	return quietFloor
}

type policyState struct {
	present bool
	value   string
}

// readPolicy reads the marker in tx. A read error is returned as is: the
// operation fails and nothing changes.
func readPolicy(ctx context.Context, tx pgx.Tx) (policyState, error) {
	var v string
	err := tx.QueryRow(ctx, `SELECT value FROM fabric_settings WHERE key = $1`, PolicyMarkerKey).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return policyState{}, nil
	}
	if err != nil {
		return policyState{}, fmt.Errorf("read the M1 comment policy marker: %w", err)
	}
	return policyState{present: true, value: v}, nil
}

// enforcedFor decides, in the deciding transaction, whether the policy
// governs a ticket_comment Submit, Retry or Dispatch (the build-and-marker
// table). It returns a refusal when the operation must not proceed at all.
func (b *Broker) enforcedFor(ctx context.Context, tx pgx.Tx) (bool, error) {
	p, err := readPolicy(ctx, tx)
	if err != nil {
		return false, err
	}
	switch {
	case p.present && p.value == PolicyMarkerM1:
		return true, nil
	case p.present:
		return false, &PolicyRefusal{CodePolicyUnknown, fmt.Sprintf("fabric_settings comment_policy holds an unknown value (%d bytes)", len(p.value))}
	case b.m1:
		return false, &PolicyRefusal{CodeMarkerMissing, "this is an M1 build and the database has no comment_policy marker; run `fab db roles --comment-policy m1`"}
	default:
		return false, nil
	}
}

// guardOn: whether Reconcile applies the early-readback guard. Any build
// that sees a marker, and every M1 build, keeps the guard (the safe side).
func (b *Broker) guardOn(ctx context.Context, tx pgx.Tx) (bool, error) {
	p, err := readPolicy(ctx, tx)
	if err != nil {
		return false, err
	}
	return b.m1 || p.present, nil
}

// checkGoverned applies the Retry and Dispatch rules to a row locked FOR
// UPDATE in tx (AC-32).
func (b *Broker) checkGoverned(ctx context.Context, tx pgx.Tx, e Effect) error {
	if e.Kind != GovernedKind {
		return nil
	}
	on, err := b.enforcedFor(ctx, tx)
	if err != nil || !on {
		return err
	}
	if e.IdemKey != M1Key(e.Mission) {
		return &PolicyRefusal{CodeNonCanonical, fmt.Sprintf("effect %s does not carry its mission's canonical key", e.ID)}
	}
	if e.Dispatches > 0 {
		return &PolicyRefusal{CodeNoRedispatch, fmt.Sprintf("effect %s was dispatched %d time(s); an M1 comment is sent at most once", e.ID, e.Dispatches)}
	}
	return nil
}

// m1MissionLockNS keys the transaction-scoped advisory lock under which
// Submit counts a mission's ticket_comment rows (namespace fabric.m1.mission).
const m1MissionLockSQL = `SELECT pg_advisory_xact_lock(hashtext('fabric.m1.mission'), hashtext($1))`

// checkSubmit applies the Submit rule to a new governed intent in tx.
func (b *Broker) checkSubmit(ctx context.Context, tx pgx.Tx, spec IntentSpec) error {
	if spec.Kind != GovernedKind {
		return nil
	}
	on, err := b.enforcedFor(ctx, tx)
	if err != nil || !on {
		return err
	}
	if spec.IdemKey != M1Key(spec.Mission) {
		return &PolicyRefusal{CodeNonCanonical, fmt.Sprintf("mission %s has one comment identity; this key is not it", spec.Mission)}
	}
	if _, err := tx.Exec(ctx, m1MissionLockSQL, string(spec.Mission)); err != nil {
		return err
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM effects WHERE mission_id = $1 AND kind = $2`, string(spec.Mission), GovernedKind).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return &PolicyRefusal{CodeSecondEffect, fmt.Sprintf("mission %s already has %d ticket_comment effect(s)", spec.Mission, n)}
	}
	return nil
}

// quietWindowPassed: the effect's last transport deadline is more than the
// quiet window in the past, on the database clock. A dispatched effect with
// no stamped deadline never passes (the safe side).
func (b *Broker) quietWindowPassed(ctx context.Context, tx pgx.Tx, e Effect) (bool, error) {
	if e.Dispatches == 0 {
		return true, nil // nothing was ever sent
	}
	var passed *bool
	err := tx.QueryRow(ctx, `SELECT now() >= max(deadline) + $2::interval FROM effect_dispatches WHERE effect_id = $1`,
		e.ID, interval(b.quietWindow)).Scan(&passed)
	if err != nil {
		return false, err
	}
	return passed != nil && *passed, nil
}

// quietPassedNow is quietWindowPassed in its own read-only transaction.
func (b *Broker) quietPassedNow(ctx context.Context, e Effect) (bool, error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck
	return b.quietWindowPassed(ctx, tx, e)
}

// CheckPolicy is the start-time check (spec table, last column): fabd's
// start and every mutating `fab effect` subcommand call it once. An M1 build
// refuses without the marker or with an unknown value; a non-M1 build is
// allowed (its decisions still honour the marker).
func (b *Broker) CheckPolicy(ctx context.Context) error {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck
	p, err := readPolicy(ctx, tx)
	if err != nil {
		return err
	}
	if !b.m1 {
		return nil
	}
	switch {
	case !p.present:
		return &PolicyRefusal{CodeMarkerMissing, "this is an M1 build and the database has no comment_policy marker; run `fab db roles --comment-policy m1`"}
	case p.value != PolicyMarkerM1:
		return &PolicyRefusal{CodePolicyUnknown, fmt.Sprintf("fabric_settings comment_policy holds an unknown value (%d bytes)", len(p.value))}
	}
	return nil
}

// ReadMarker reports the marker row for `fab policy marker` (goodqa#16039).
func ReadMarker(ctx context.Context, b *Broker) (value string, present bool, err error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck
	p, err := readPolicy(ctx, tx)
	return p.value, p.present, err
}

// CommentFinder is a read-only readback that names the remote comment that
// carries an effect's key (AC-31 late_write). GoodQA and Fake implement it.
type CommentFinder interface {
	FindComment(ctx context.Context, e Effect) (commentID string, found bool, err error)
}

// LoadKey reads the effect stored under an idempotency key (pgx.ErrNoRows
// when none).
func (b *Broker) LoadKey(ctx context.Context, key string) (Effect, error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return Effect{}, err
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck
	return b.loadTx(ctx, tx, "idem_key", key)
}

// PolicyEnforced reports whether this broker enforces the M1 comment policy
// on this database right now (the AC-31 loop runs only then).
func (b *Broker) PolicyEnforced(ctx context.Context) (bool, error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck
	p, err := readPolicy(ctx, tx)
	if err != nil {
		return false, err
	}
	return b.m1 || (p.present && p.value == PolicyMarkerM1), nil
}
