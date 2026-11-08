package metrics

import "testing"

// goodqa#15324 S1b: ladder missions (m_ladder-<task>) hold receipts, not work.
// They must not inflate the mission gauge.
func TestMissionGaugeIgnoresLadderMissions(t *testing.T) {
	pool := open(t)
	exec(t, pool,
		`INSERT INTO missions (id, ticket, execution, terminal, state, provider) VALUES
			('m_ladder-t1', 'ladder-receipts', 'IDLE', 'NONE', '{}', 'anthropic-api'),
			('m_ladder-t2', 'ladder-receipts', 'IDLE', 'NONE', '{}', 'anthropic-api')`)
	out := render(t, pool, opts)
	if got := sample(out, `fabric_missions{execution="IDLE",terminal="NONE"}`); got != "absent" {
		t.Fatalf("ladder missions counted: fabric_missions IDLE/NONE = %s", got)
	}
	exec(t, pool, `INSERT INTO missions (id, ticket, execution, terminal, state, provider) VALUES
		('m_real', 'goodqa#1', 'IDLE', 'NONE', '{}', 'claude-code')`)
	out = render(t, pool, opts)
	if got := sample(out, `fabric_missions{execution="IDLE",terminal="NONE"}`); got != "1" {
		t.Fatalf("real mission not counted alone: %s (want 1)", got)
	}
	// PL-11: a ladder row in a bad state is a fault and stays counted.
	exec(t, pool, `UPDATE missions SET execution = 'ADMITTED' WHERE id = 'm_ladder-t1'`)
	out = render(t, pool, opts)
	if got := sample(out, `fabric_missions{execution="ADMITTED",terminal="NONE"}`); got != "1" {
		t.Fatalf("an ADMITTED ladder row must be counted: %s (want 1)", got)
	}
}
