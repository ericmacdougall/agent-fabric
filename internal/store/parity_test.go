package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"fabric/internal/domain"
	"fabric/internal/reducer"
	"fabric/internal/reducer/corpus"
)

// seedCorpus stores every corpus log through plain INSERTs (events and
// outbox) and the snapshot the permissive fold gives, as the writer left it.
func seedCorpus(t *testing.T, st *Store, ctx context.Context) {
	t.Helper()
	ids := make([]string, 0)
	logs := corpus.Logs()
	for id := range logs {
		ids = append(ids, string(id))
	}
	sort.Strings(ids)
	at := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	for _, sid := range ids {
		id := domain.MissionID(sid)
		mk(t, st, ctx, id)
		events := logs[id]
		for i, e := range events {
			payload, err := json.Marshal(e)
			if err != nil {
				t.Fatal(err)
			}
			when := at.Add(time.Duration(i) * 1500 * time.Microsecond)
			if _, err := st.pool.Exec(ctx, `INSERT INTO events (mission_id, seq, kind, actor, command_id, payload, at) VALUES ($1, $2, $3, 'corpus', $4, $5, $6)`,
				sid, e.Seq, string(e.Kind), fmt.Sprintf("c%d", e.Seq), payload, when); err != nil {
				t.Fatal(err)
			}
			if _, err := st.pool.Exec(ctx, `INSERT INTO outbox (mission_id, seq, kind, payload, at) VALUES ($1, $2, $3, $4, $5)`, sid, e.Seq, string(e.Kind), payload, when); err != nil {
				t.Fatal(err)
			}
		}
		state, err := json.Marshal(corpus.Fold(id, events))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.pool.Exec(ctx, `UPDATE missions SET state = $2, seq = $3 WHERE id = $1`, sid, state, events[len(events)-1].Seq); err != nil {
			t.Fatal(err)
		}
	}
}

// Every mission of the synthetic corpus rebuilds identically through the
// upcaster path: the digest of the rebuilt state equals the golden digest the
// pre-change binary produced (internal/reducer/corpus/testdata, generated at
// 207254dab3d4), the digest of the stored snapshot, and SQL state = rebuilt
// (jsonb equality, never a text comparison).
func TestV1ReplayParity(t *testing.T) {
	st, ctx := openTest(t)
	seedCorpus(t, st, ctx)
	b, err := os.ReadFile("../reducer/corpus/testdata/parity-golden.json")
	if err != nil {
		t.Fatal(err)
	}
	golden := map[string]string{}
	if err := json.Unmarshal(b, &golden); err != nil {
		t.Fatal(err)
	}
	if len(golden) != len(corpus.Logs()) || len(golden) < 20 {
		t.Fatalf("golden holds %d digests for %d missions", len(golden), len(corpus.Logs()))
	}
	for sid, want := range golden {
		id := domain.MissionID(sid)
		rebuilt, events, err := st.VerifiedSnapshot(ctx, id)
		if err != nil {
			t.Fatalf("%s: %v", sid, err)
		}
		if got := reducer.Digest(rebuilt); got != want {
			t.Errorf("%s: rebuilt digest %s, golden %s", sid, got, want)
		}
		if len(events) != len(corpus.Logs()[id]) {
			t.Errorf("%s: %d events through the reader, %d stored", sid, len(events), len(corpus.Logs()[id]))
		}
		state, _ := json.Marshal(rebuilt)
		var eq bool
		if err := st.pool.QueryRow(ctx, `SELECT state = $2::jsonb FROM missions WHERE id = $1`, sid, state).Scan(&eq); err != nil || !eq {
			t.Errorf("%s: stored state is not jsonb-equal to the rebuilt state (%v)", sid, err)
		}
	}
}

// The view built from the events row (replay) and from the joined outbox row
// (consumer) are byte-equal.
func TestViewPathsAgree(t *testing.T) {
	st, ctx := openTest(t)
	seedCorpus(t, st, ctx)
	for _, sid := range []string{"m_corpus_all", "m_corpus_delivered"} {
		views, err := readViews(ctx, st.pool, domain.MissionID(sid))
		if err != nil {
			t.Fatal(err)
		}
		want := map[int64]string{}
		for _, v := range views {
			c, err := v.Canonical()
			if err != nil {
				t.Fatal(err)
			}
			want[v.Seq] = string(c)
		}
		page, err := st.OutboxPage(ctx, "viewpaths-"+sid, 10000)
		if err != nil {
			t.Fatal(err)
		}
		seen := 0
		for _, r := range page {
			if string(r.Mission) != sid {
				continue
			}
			c, err := r.View.Canonical()
			if err != nil {
				t.Fatal(err)
			}
			if string(c) != want[r.Seq] {
				t.Errorf("%s seq %d: outbox view and events view differ", sid, r.Seq)
			}
			seen++
		}
		if seen != len(views) {
			t.Errorf("%s: %d outbox views, %d events views", sid, seen, len(views))
		}
	}
}
