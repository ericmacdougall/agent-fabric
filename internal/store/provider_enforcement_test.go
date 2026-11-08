package store

import (
	"strings"
	"testing"
)

func TestAgentRunsDatabaseProviderEnforcement(t *testing.T) {
	st, ctx := openTest(t)
	for _, q := range []string{
		`INSERT INTO missions (id, ticket, state, provider) VALUES
			('m_native', 'goodqa#1', '{}', 'anthropic-api'),
			('m_claude', 'goodqa#2', '{}', 'claude-code')`,
		`INSERT INTO attempts (id, mission_id, fence, revision, generation, state, path, lock_path) VALUES
			('r_native', 'm_native', 1, 0, 0, 'RUNNING', 'anthropic-api', '/tmp/native-locks/r_native.lock'),
			('r_claude', 'm_claude', 1, 0, 0, 'RUNNING', 'claude-code', NULL)`,
	} {
		if _, err := st.Pool().Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	_, err := st.Pool().Exec(ctx, `INSERT INTO agent_runs (attempt_id, fence, config_sha, billing_mode) VALUES ('r_claude', 1, 'old-binary', 'api_key')`)
	if err == nil || !strings.Contains(err.Error(), "anthropic-api") {
		t.Fatalf("old binary attached agent_runs to claude-code attempt: %v", err)
	}
	if _, err := st.Pool().Exec(ctx, `INSERT INTO agent_runs (attempt_id, fence, config_sha, billing_mode) VALUES ('r_native', 1, 'native', 'api_key')`); err != nil {
		t.Fatalf("native attempt rejected: %v", err)
	}
	if _, err := st.Pool().Exec(ctx, `UPDATE agent_runs SET attempt_id = 'r_claude' WHERE attempt_id = 'r_native'`); err == nil {
		t.Fatal("agent_runs row moved to a claude-code attempt")
	}
}

func TestMigration0032LocksRunsBeforeValidationAndTrigger(t *testing.T) {
	sql, err := migrationFS.ReadFile("migrations/0032_provider_enforcement.sql")
	if err != nil {
		t.Fatal(err)
	}
	text := string(sql)
	lock := strings.Index(text, "LOCK TABLE public.agent_runs IN ACCESS EXCLUSIVE MODE;")
	validate := strings.Index(text, "DO $$")
	trigger := strings.Index(text, "CREATE TRIGGER agent_runs_provider")
	if lock < 0 || validate < 0 || trigger < 0 || !(lock < validate && validate < trigger) {
		t.Fatalf("0032 must lock writes before validation and trigger installation: lock=%d validate=%d trigger=%d", lock, validate, trigger)
	}
}

func TestMigration0032RejectsExistingCrossProviderRun(t *testing.T) {
	st, ctx, _ := freshStore(t, level26)
	mustExec(t, st, ctx,
		`INSERT INTO missions (id, ticket, state, provider) VALUES ('m_claude', 'goodqa#2', '{}', 'claude-code')`,
		`INSERT INTO attempts (id, mission_id, fence, revision, generation, state, path) VALUES
			('r_claude', 'm_claude', 1, 0, 0, 'RUNNING', 'claude-code')`,
		`INSERT INTO agent_runs (attempt_id, fence, config_sha, billing_mode) VALUES ('r_claude', 1, 'old-binary', 'api_key')`,
	)
	if err := st.Migrate(ctx); err == nil || !strings.Contains(err.Error(), "0032") || !strings.Contains(err.Error(), "r_claude") {
		t.Fatalf("migration accepted an existing cross-provider agent_runs row: %v", err)
	}
}
