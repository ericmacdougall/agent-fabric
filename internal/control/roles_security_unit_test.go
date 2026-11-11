package control

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type settingRow struct {
	values []any
	err    error
}

func (r settingRow) Scan(dst ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dst) != len(r.values) {
		return errors.New("unexpected setting query")
	}
	for i, value := range r.values {
		switch p := dst[i].(type) {
		case *string:
			*p = value.(string)
		case *bool:
			*p = value.(bool)
		case **string:
			v := value.(string)
			*p = &v
		default:
			return errors.New("unexpected setting destination")
		}
	}
	return nil
}

type protectionTx struct {
	pgx.Tx
	activity                                    string
	utility                                     string
	denyActivity, ignoreActivity, ignoreUtility bool
	calls                                       []string
}

func (t *protectionTx) Exec(_ context.Context, q string, _ ...any) (pgconn.CommandTag, error) {
	t.calls = append(t.calls, q)
	if q == "SET LOCAL track_activities = off" {
		if t.denyActivity {
			return pgconn.CommandTag{}, errors.New("permission denied")
		}
		if !t.ignoreActivity {
			t.activity = "off"
		}
	}
	if q == "SET LOCAL pg_stat_statements.track_utility = off" && !t.ignoreUtility {
		t.utility = "off"
	}
	return pgconn.NewCommandTag("SET"), nil
}
func (t *protectionTx) QueryRow(_ context.Context, q string, _ ...any) pgx.Row {
	switch q {
	case "SELECT current_setting('track_activities')":
		return settingRow{values: []any{t.activity}}
	case "SELECT rolsuper FROM pg_roles WHERE rolname = current_user":
		return settingRow{values: []any{true}}
	case "SELECT current_setting('log_min_error_statement'), current_setting('log_statement'), current_setting('log_min_duration_statement')":
		return settingRow{values: []any{"panic", "none", "-1"}}
	case "SELECT current_setting('pg_stat_statements.track_utility', true)", "SELECT current_setting('pg_stat_statements.track_utility')":
		return settingRow{values: []any{t.utility}}
	}
	return settingRow{err: errors.New("unexpected setting query")}
}
func TestPasswordProtectionRequiresVerifiedLocalActivity(t *testing.T) {
	// These legacy fake responses lack the required destination/module profile.
	// They must fail closed before even a password-free handoff is attempted.
	for _, tx := range []*protectionTx{{activity: "on", utility: "on"}, {activity: "on", denyActivity: true}, {activity: "on", ignoreActivity: true}, {activity: "on", utility: "on", ignoreUtility: true}} {
		if err := requireQuietLogging(context.Background(), tx); err == nil {
			t.Error("incomplete protection profile was accepted")
		}
	}
}
