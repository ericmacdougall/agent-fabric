package store

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"fabric/internal/domain"
)

func plantBad(t *testing.T, st *Store, ctx context.Context, m domain.MissionID, seq int64, kind, payload string) {
	t.Helper()
	if _, err := st.pool.Exec(ctx, `INSERT INTO events (mission_id, seq, kind, actor, command_id, payload) VALUES ($1, $2, $3, 'test', $4, $5::jsonb)`,
		string(m), seq, kind, fmt.Sprintf("bad-%s-%d", m, seq), payload); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `INSERT INTO outbox (mission_id, seq, kind, payload) VALUES ($1, $2, $3, $4::jsonb)`, string(m), seq, kind, payload); err != nil {
		t.Fatal(err)
	}
}

// `fab streams held` lists a mission, an event kind name and a reason class,
// never payload text; the scan finds a mission that is not held yet; the
// gauge counts distinct missions.
func TestStreamsHeld(t *testing.T) {
	st, ctx := openTest(t)
	for _, id := range []domain.MissionID{"m_sh_a", "m_sh_b", "m_sh_c"} {
		mk(t, st, ctx, id)
		ok(t, st, ctx, "chat", "admit-"+string(id), domain.Command{Kind: domain.CmdAdmitMission, Mission: id})
	}
	const canary = "CANARY-TOKEN-do-not-print"
	plantBad(t, st, ctx, "m_sh_a", 50, "NEW_FUTURE_KIND", `{"kind":"NEW_FUTURE_KIND","mission":"m_sh_a","seq":50,"reason":"`+canary+`"}`)
	plantBad(t, st, ctx, "m_sh_b", 50, "ATTEMPT_STARTED", `{"kind":"ATTEMPT_STARTED","mission":"m_sh_b","seq":50,"fence":"`+canary+`"}`)
	for _, m := range []domain.MissionID{"m_sh_a", "m_sh_b"} {
		if _, err := st.pool.Exec(ctx, `INSERT INTO outbox_cursors (consumer, last_id) SELECT $1, id - 1 FROM outbox WHERE mission_id = $2 AND seq = 50 ON CONFLICT DO NOTHING`, HoldPrefix+string(m), string(m)); err != nil {
			t.Fatal(err)
		}
	}

	infos, err := st.HeldInfos(ctx, "", 100)
	if err != nil || len(infos) != 2 {
		t.Fatalf("HeldInfos: %+v %v", infos, err)
	}
	if infos[0].Reason != "unknown-kind" || infos[0].Kind != "NEW_FUTURE_KIND" || infos[1].Reason != "undecodable" || infos[1].Kind != "ATTEMPT_STARTED" {
		t.Fatalf("kind or reason wrong: %+v", infos)
	}
	scanned, next, err := st.ScanUndecodable(ctx, "", 100)
	if err != nil || next == "" || len(scanned) != 2 {
		t.Fatalf("ScanUndecodable: %+v next=%q err=%v", scanned, next, err)
	}
	for _, h := range scanned {
		if !h.Held {
			t.Fatalf("%s should be held", h.Mission)
		}
	}
	plantBad(t, st, ctx, "m_sh_c", 50, "INTENT_REVISED", `{"kind":"INTENT_REVISED","mission":"m_sh_c","seq":"x"}`)
	scanned, _, err = st.ScanUndecodable(ctx, "", 100)
	if err != nil || len(scanned) != 3 {
		t.Fatalf("scan after planting: %+v %v", scanned, err)
	}
	for _, h := range scanned {
		if h.Mission == "m_sh_c" && h.Held {
			t.Fatal("m_sh_c is not held yet")
		}
	}
	for _, h := range append(infos, scanned...) {
		if s := fmt.Sprintf("%+v", h); strings.Contains(s, canary) {
			t.Fatalf("payload text leaked: %s", s)
		}
	}
	if n, err := st.HeldCount(ctx); err != nil || n != 2 {
		t.Fatalf("held count %d %v", n, err)
	}
}
