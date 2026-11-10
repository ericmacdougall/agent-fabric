package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestUnknownKeysNeverEcho(t *testing.T) {
	for _, key := range []string{"password_" + strings.Repeat("X", 20), strings.Repeat("x", 100), "bad\nkey", "密钥"} {
		for _, nested := range []string{"", "actor", "subject"} {
			m := map[string]any{"schema": Schema, "id": "harness:unknown", "ts": "2026-11-09T12:30:45Z", "kind": "wal"}
			if nested == "" {
				m[key] = "x"
			} else {
				m[nested] = map[string]any{key: "x"}
			}
			line, _ := json.Marshal(m)
			b, err := Parse(strings.NewReader(string(line)), "harness")
			if err != nil || b.Lines != 1 || len(b.Accepted) != 0 || len(b.Rejected) != 1 {
				t.Fatal("unexpected rejection classification")
			}
			want := "unknown-field"
			if nested != "" {
				want += ":" + nested
			}
			if b.Rejected[0].Reason != want {
				t.Error("unknown key entered reason or reason was not fixed")
			}
		}
	}
}

func TestMarkerCannotExemptCredential(t *testing.T) {
	for _, marker := range []string{"[REDACTED:", "[REDACTED:named-credential]", "[REDACTED:unknown]"} {
		secret := "password=" + marker + strings.Repeat("Q", 18)
		got, n := Redact(secret)
		if n == 0 || strings.Contains(got, strings.Repeat("Q", 18)) {
			t.Error("marker-bearing credential survived")
		}
		again, n := Redact(got)
		if got != again || n != 0 {
			t.Error("generated placeholder was not stable")
		}
		m := map[string]any{"schema": Schema, "id": "harness:marker", "ts": "2026-11-09T12:30:45Z", "kind": "wal", "host": secret, "tenant": secret, "summary": secret,
			"actor": map[string]string{"agent": secret, "runtime": secret, "session": secret}, "subject": map[string]string{"repo": secret, "branch": secret, "ref": secret, "ticket": secret, "initiative": secret}}
		line, _ := json.Marshal(m)
		ev, err := ValidateLine(line, "harness")
		if err != nil {
			t.Fatal("marker fixture refused")
		}
		encoded, _ := json.Marshal(ev)
		if strings.Contains(string(encoded), strings.Repeat("Q", 18)) || ev.Redacted != 11 {
			t.Error("free text credential survived validation")
		}
	}
	for _, p := range secretPatterns {
		placeholder := "[REDACTED:" + p.kind + "]"
		if got, n := Redact(placeholder); got != placeholder || n != 0 {
			t.Error("ordinary placeholder changed")
		}
	}
}

type deadlineDB struct {
	tx             *deadlineTx
	notifyDeadline time.Time
	notifyCalled   bool
}

func (d *deadlineDB) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) { return d.tx, nil }
func (d *deadlineDB) Exec(ctx context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	d.notifyCalled = true
	d.notifyDeadline, _ = ctx.Deadline()
	<-ctx.Done()
	return pgconn.CommandTag{}, ctx.Err()
}

type deadlineTx struct {
	pgx.Tx
	rollbackDeadline time.Time
	commitErr        error
	commitDelay      time.Duration
	insertErr        error
}

func (t *deadlineTx) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.NewCommandTag("INSERT 0 1"), t.insertErr
}
func (t *deadlineTx) Commit(ctx context.Context) error {
	timer := time.NewTimer(t.commitDelay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return t.commitErr
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (t *deadlineTx) Rollback(ctx context.Context) error {
	t.rollbackDeadline, _ = ctx.Deadline()
	return nil
}
func TestStoreNotificationUsesOriginalDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	deadline, _ := ctx.Deadline()
	db := &deadlineDB{tx: &deadlineTx{commitDelay: 40 * time.Millisecond}}
	start := time.Now()
	got, err := Store(ctx, db, []Event{{ID: "harness:deadline"}})
	if err != nil || got.Inserted != 1 {
		t.Fatal("confirmed commit incorrectly failed after hint failure")
	}
	if !db.notifyCalled || db.notifyDeadline.IsZero() || db.notifyDeadline.After(deadline) {
		t.Error("notification escaped original deadline")
	}
	if db.tx.rollbackDeadline.IsZero() || db.tx.rollbackDeadline.After(deadline) {
		t.Error("rollback escaped original deadline")
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Error("notification extended caller budget")
	}
}
func TestStoreUncertainCommitIsError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	db := &deadlineDB{tx: &deadlineTx{commitErr: context.DeadlineExceeded}}
	got, err := Store(ctx, db, []Event{{ID: "harness:uncertain"}})
	if !errors.Is(err, context.DeadlineExceeded) || got.Inserted != 0 || db.notifyCalled {
		t.Error("uncertain commit reported as confirmed or sent a hint")
	}
}
