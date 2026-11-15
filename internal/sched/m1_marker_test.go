package sched

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"fabric/internal/domain"
	"fabric/internal/store"
)

func setM1Marker(t *testing.T, st *store.Store) {
	t.Helper()
	if _, err := st.Pool().Exec(context.Background(), `INSERT INTO fabric_settings (key, value) VALUES ('comment_policy', 'm1')
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`); err != nil {
		t.Fatal(err)
	}
}

func missionActivity(t *testing.T, st *store.Store, m domain.MissionID) (attempts, events int) {
	t.Helper()
	ctx := context.Background()
	st.Pool().QueryRow(ctx, `SELECT count(*) FROM attempts WHERE mission_id = $1`, string(m)).Scan(&attempts) //nolint:errcheck
	st.Pool().QueryRow(ctx, `SELECT count(*) FROM events WHERE mission_id = $1`, string(m)).Scan(&events)     //nolint:errcheck
	return attempts, events
}

// goodqa#15966 item 4(b), PR erratum E12: a fabd without the M1 token
// launch (Credential nil) on a database with the comment_policy=m1 marker
// refuses before START: no attempt, no event, 0 processes. The marker is
// read at New and again on every tick.
func TestM1MarkedDatabaseRefusesStartWithoutCredential(t *testing.T) {
	t.Run("marked before New", func(t *testing.T) {
		const m = domain.MissionID("m_mk1")
		st, ln, cfg := ownerSetup(t, m)
		setM1Marker(t, st)
		_, ev0 := missionActivity(t, st, m)
		s := New(st, cfg, ln)
		err := s.runOnce(context.Background(), m)
		if err == nil || !strings.Contains(err.Error(), classMarkedWithoutCredential) {
			t.Fatalf("runOnce: %v, want a %s refusal", err, classMarkedWithoutCredential)
		}
		if a, ev := missionActivity(t, st, m); a != 0 || ev != ev0 || atomic.LoadInt32(&ln.starts) != 0 {
			t.Fatalf("attempts %d, events %d -> %d, starts %d", a, ev0, ev, ln.starts)
		}
	})
	t.Run("marked while running", func(t *testing.T) {
		const m = domain.MissionID("m_mk2")
		st, ln, cfg := ownerSetup(t, m)
		s := New(st, cfg, ln)
		setM1Marker(t, st)
		if _, err := s.Tick(context.Background()); err != nil && !strings.Contains(err.Error(), classMarkedWithoutCredential) {
			t.Fatal(err)
		}
		if a, _ := missionActivity(t, st, m); a != 0 || atomic.LoadInt32(&ln.starts) != 0 {
			t.Fatalf("a marker set while fabd runs was not seen within one tick: %d attempts, %d starts", a, ln.starts)
		}
	})
}

// The pre-M1 behaviour is unchanged: on an unmarked database a fabd with
// no Credential still STARTs.
func TestUnmarkedDatabaseStartsWithoutCredential(t *testing.T) {
	const m = domain.MissionID("m_mk3")
	st, ln, cfg := ownerSetup(t, m)
	s := New(st, cfg, ln)
	if err := s.runOnce(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if a, _ := missionActivity(t, st, m); a != 1 {
		t.Fatalf("%d attempts, want the START", a)
	}
}
