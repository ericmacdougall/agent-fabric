package store

import (
	"strings"
	"testing"
)

// goodqa#15361 N3: agent_compactions is append-only, rejects an empty summary
// and a kept index past its own index, and its file re-applies cleanly.
func TestMigrate0039AgentCompactions(t *testing.T) {
	st, ctx, _ := freshStore(t, migration0037)
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	mustExec(t, st, ctx,
		`INSERT INTO missions (id, ticket, state, provider) VALUES ('m_cmp', 'goodqa#15361', '{}', 'anthropic-api')`,
		`INSERT INTO attempts (id, mission_id, fence, revision, generation, state, path, lock_path) VALUES
			('r_cmp', 'm_cmp', 1, 0, 0, 'RUNNING', 'anthropic-api', '/tmp/native-locks/r_cmp.lock')`,
		`INSERT INTO agent_compactions (attempt_id, idx, first_kept_idx, summary, request_sha, cost_micros, fence)
			VALUES ('r_cmp', 6, 3, 'S', 'sha', 10, 1)`)
	for name, q := range map[string]string{
		"update":        `UPDATE agent_compactions SET summary = 'x'`,
		"delete":        `DELETE FROM agent_compactions`,
		"empty summary": `INSERT INTO agent_compactions (attempt_id, idx, first_kept_idx, summary, request_sha, cost_micros, fence) VALUES ('r_cmp', 8, 3, '', 'sha', 1, 1)`,
		"kept past idx": `INSERT INTO agent_compactions (attempt_id, idx, first_kept_idx, summary, request_sha, cost_micros, fence) VALUES ('r_cmp', 8, 9, 'S', 'sha', 1, 1)`,
		"duplicate idx": `INSERT INTO agent_compactions (attempt_id, idx, first_kept_idx, summary, request_sha, cost_micros, fence) VALUES ('r_cmp', 6, 3, 'S', 'sha', 1, 1)`,
	} {
		if _, err := st.pool.Exec(ctx, q); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	sql := mustRead(t, "migrations/0039_agent_compactions.sql")
	if !strings.Contains(sql, "SET LOCAL lock_timeout") {
		t.Fatal("0039 does not set its lock_timeout")
	}
	if _, err := st.pool.Exec(ctx, sql); err != nil {
		t.Fatalf("re-applying 0039: %v", err)
	}
	if n := count(t, st, ctx, `SELECT count(*) FROM agent_compactions`); n != 1 {
		t.Fatalf("%d rows after re-apply", n)
	}
}
