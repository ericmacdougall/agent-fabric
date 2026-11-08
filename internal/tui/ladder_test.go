package tui

import (
	"context"
	"testing"
	"time"
)

// goodqa#15324 S1b: the mission pane never lists ladder missions.
func TestMissionPaneHidesLadderMissions(t *testing.T) {
	pool, sess := open(t, 5*time.Second)
	exec(t, pool, `INSERT INTO missions (id, ticket, execution, terminal, state, provider) VALUES
		('m_ladder-t1', 'ladder-receipts', 'IDLE', 'NONE', '{}', 'anthropic-api'),
		('m_real', 'goodqa#1', 'IDLE', 'NONE', '{}', 'claude-code')`)
	d, _, err := sess.Src.Fetch(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Missions) != 1 || d.Missions[0].ID != "m_real" {
		t.Fatalf("mission pane = %+v, want only m_real", d.Missions)
	}
	// PL-11: a ladder row in a bad state stays visible, with its ladder-receipts ticket as the tag.
	exec(t, pool, `UPDATE missions SET execution = 'ADMITTED' WHERE id = 'm_ladder-t1'`)
	d, _, err = sess.Src.Fetch(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range d.Missions {
		found = found || (m.ID == "m_ladder-t1" && m.Ticket == "ladder-receipts")
	}
	if !found {
		t.Fatalf("an ADMITTED ladder row must be shown: %+v", d.Missions)
	}
}
