package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"fabric/internal/domain"
	"fabric/internal/ladder"
)

// goodqa#15324 S1b: ladder records through the real Execute path (Postgres).

func lp(v string) *string { return &v }

func lhex(n int) string { return fmt.Sprintf("%064x", n) }

// lrsha varies in the leading digits: the command key uses the first 62 hex.
func lrsha(n int) string { return fmt.Sprintf("%08x", n) + strings.Repeat("a", 56) }

func lfail(n int) domain.LadderRecord {
	return domain.LadderRecord{Class: "FAIL", Step: "review", Fp: lp(lhex(n % 7)), Rsha: lp(lrsha(1000 + n)), Launched: true}
}

func lclaim(h string) domain.LadderRecord {
	return domain.LadderRecord{Class: "CLAIM", Step: "claim", Commit: lp(h)}
}

func lspend(micros, ord int64) domain.LadderRecord {
	return domain.LadderRecord{Class: "NOT_COUNTED", Step: "spend", SpendMicros: micros, Ordinal: ord}
}

func lapprove(ask string) domain.LadderRecord {
	return domain.LadderRecord{Step: "approve_top", Approval: &domain.LadderApproval{Ask: ask, Src: "user:s/u \"ok\"", By: "op:eric"}}
}

func countAll(t *testing.T, st *Store, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := st.pool.QueryRow(t.Context(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func TestLadderFirstRecordCreatesIdleMissionWithoutASlot(t *testing.T) {
	st, ctx := openTest(t)
	if err := st.SetCap(ctx, 1); err != nil {
		t.Fatal(err)
	}
	resp, err := st.RecordLadderReceipt(ctx, "goodqa-15299", lfail(1))
	if err != nil || len(resp.Events) != 1 || resp.Events[0].Kind != domain.EvLadderReceipt {
		t.Fatalf("record: (%+v, %v)", resp, err)
	}
	var provider, exec, term, ticket, repo string
	if err := st.pool.QueryRow(ctx, `SELECT provider, execution, terminal, ticket, repo FROM missions WHERE id = 'm_ladder-goodqa-15299'`).
		Scan(&provider, &exec, &term, &ticket, &repo); err != nil {
		t.Fatal(err)
	}
	if provider != "anthropic-api" || exec != "IDLE" || term != "NONE" || ticket != "ladder-receipts" || repo != "" {
		t.Fatalf("ladder mission row: %s %s %s %s %q", provider, exec, term, ticket, repo)
	}
	if n, _ := st.SlotsHeld(ctx); n != 0 {
		t.Fatalf("the ladder mission holds %d slots", n)
	}
	// Cap is 1 and the ladder mission exists: a real mission still admits.
	if err := st.CreateMission(ctx, "m_real", domain.ProviderClaudeCode, "GoodQA goodqa#1", "", ""); err != nil {
		t.Fatal(err)
	}
	ok(t, st, ctx, "chat", "admit", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_real"})
	// The snapshot replays to itself: events and snapshot agree.
	if _, err := st.Rebuild(ctx, "m_ladder-goodqa-15299"); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
}

// PL2-2: an approval before any receipt creates the row, as in Python.
func TestLadderApprovalFirstCreatesTheRow(t *testing.T) {
	st, ctx := openTest(t)
	resp, err := st.RecordLadderReceipt(ctx, "t0", lapprove("a_1"))
	if err != nil || len(resp.Events) != 1 || resp.Events[0].Kind != domain.EvLadderTopApproved {
		t.Fatalf("approval first: (%+v, %v)", resp, err)
	}
	if countAll(t, st, "SELECT count(*) FROM missions WHERE id = 'm_ladder-t0'") != 1 {
		t.Fatal("no ladder row")
	}
}

func TestLadderMissionRefusesOtherCommandsAndRealMissionsRefuseRecords(t *testing.T) {
	st, ctx := openTest(t)
	if _, err := st.RecordLadderReceipt(ctx, "t1", lclaim("abcdef0")); err != nil {
		t.Fatal(err)
	}
	before := countAll(t, st, "SELECT count(*) FROM events")
	for _, k := range []domain.CommandKind{domain.CmdAdmitMission, domain.CmdStartAttempt, domain.CmdWake, domain.CmdCompleteMission} {
		_, err := st.Execute(ctx, "chat", domain.CommandID("x-"+string(k)), domain.Command{Kind: k, Mission: "m_ladder-t1",
			Attempt: "r_1", Host: "h_t2", Role: "writer", Path: domain.ProviderAnthropicAPI})
		if !errors.Is(err, domain.ErrLadderMission) {
			t.Errorf("%s on a ladder mission: %v, want ErrLadderMission", k, err)
		}
	}
	if err := st.CreateMission(ctx, "m_real", domain.ProviderClaudeCode, "GoodQA goodqa#1", "", ""); err != nil {
		t.Fatal(err)
	}
	r := lclaim("abcdef0")
	_, err := st.Execute(ctx, domain.LadderActor, "bad", domain.Command{Kind: domain.CmdRecordLadderReceipt, Mission: "m_real", Ladder: &r})
	if !errors.Is(err, domain.ErrNotLadderMission) {
		t.Fatalf("record on a real mission: %v, want ErrNotLadderMission", err)
	}
	// Only the ladder actor writes ladder records.
	_, err = st.Execute(ctx, "op:eric", "other", domain.Command{Kind: domain.CmdRecordLadderReceipt, Mission: "m_ladder-t1", Ladder: &r})
	if !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("record by another actor: %v, want ErrInvalidTransition", err)
	}
	if n := countAll(t, st, "SELECT count(*) FROM events"); n != before {
		t.Fatalf("events %d -> %d after refusals", before, n)
	}
	if n := countAll(t, st, "SELECT count(*) FROM events WHERE mission_id = 'm_real'"); n != 0 {
		t.Fatalf("%d events stored on a real mission", n)
	}
}

// PL-4 and the id-hijack case: the prefix is reserved in every creator, and an
// existing non-ladder row is never adopted.
func TestLadderPrefixIsReserved(t *testing.T) {
	st, ctx := openTest(t)
	for _, p := range []domain.Provider{domain.ProviderClaudeCode, domain.ProviderAnthropicAPI} {
		if err := st.CreateMission(ctx, "m_ladder-sneaky", p, "GoodQA goodqa#1", "", ""); !errors.Is(err, domain.ErrInvalidID) {
			t.Errorf("CreateMission(%s): %v, want ErrInvalidID", p, err)
		}
	}
	spec := MissionSpec{Provider: domain.ProviderClaudeCode, ID: "m_ladder-sneaky2", Ticket: "GoodQA goodqa#3", Source: "file:/x"}
	if err := st.CreateMissionSpec(ctx, spec); !errors.Is(err, domain.ErrInvalidID) {
		t.Errorf("CreateMissionSpec: %v, want ErrInvalidID", err)
	}
	if _, err := st.EnsureMissionSpec(ctx, spec); !errors.Is(err, domain.ErrInvalidID) {
		t.Errorf("EnsureMissionSpec: %v, want ErrInvalidID", err)
	}
	if n := countAll(t, st, "SELECT count(*) FROM missions WHERE starts_with(id, 'm_ladder-')"); n != 0 {
		t.Fatalf("%d ladder-prefixed missions created through a general path", n)
	}
	// A pre-existing row with the prefix that is not a ladder mission is never
	// adopted: nothing is appended to it.
	for i, q := range []string{
		`INSERT INTO missions (id, ticket, state, provider) VALUES ('m_ladder-old', 'GoodQA goodqa#9', '{"id":"m_ladder-old"}', 'claude-code')`,
		`INSERT INTO missions (id, ticket, state, provider) VALUES ('m_ladder-old2', 'GoodQA goodqa#9', '{"id":"m_ladder-old2"}', 'anthropic-api')`,
		`INSERT INTO missions (id, ticket, repo, state, provider) VALUES ('m_ladder-old3', 'ladder-receipts', 'r', '{"id":"m_ladder-old3"}', 'anthropic-api')`,
	} {
		if _, err := st.pool.Exec(ctx, q); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	for _, task := range []string{"old", "old2", "old3"} {
		if _, err := st.RecordLadderReceipt(ctx, task, lclaim("abcdef0")); !errors.Is(err, domain.ErrInvalidID) {
			t.Errorf("record onto non-ladder row %s: %v, want ErrInvalidID", task, err)
		}
	}
	if n := countAll(t, st, "SELECT count(*) FROM events WHERE starts_with(mission_id, 'm_ladder-')"); n != 0 {
		t.Fatalf("%d events appended to non-ladder rows", n)
	}
	// An over-long task is refused, not truncated.
	long := strings.Repeat("a", 56)
	if _, err := st.RecordLadderReceipt(ctx, long, lclaim("abcdef0")); !errors.Is(err, domain.ErrInvalidID) {
		t.Fatalf("56-byte task: %v, want ErrInvalidID", err)
	}
	if n := countAll(t, st, "SELECT count(*) FROM missions WHERE starts_with(id, 'm_ladder-aaaa')"); n != 0 {
		t.Fatalf("an over-long task created %d missions (truncated?)", n)
	}
}

// PL-3 and NEG-4: a refused first record stores nothing, not even the row.
func TestLadderRefusedFirstRecordLeavesNoRow(t *testing.T) {
	st, ctx := openTest(t)
	bad := []domain.LadderRecord{
		{Class: "MAYBE", Step: "review", Rsha: lp(lhex(1))},
		{Class: "FAIL", Step: "review", Rsha: lp(lhex(2)), Launched: true}, // no fingerprint
		{Class: "FAIL", Step: "review", Fp: lp("RT-1"), Rsha: lp(lhex(3)), Launched: true},
		{Class: "FAIL", Step: "review", Fp: lp(lhex(4)), Rsha: lp(lhex(5)), Commit: lp("not-a-sha"), Launched: true},
		{Step: "approve_top", Approval: &domain.LadderApproval{Ask: "a_1", Src: "inferred: from x", By: "op:eric"}},
		{Step: "approve_top", Approval: &domain.LadderApproval{Ask: "a_1", Src: "user:x", By: "chat"}},
	}
	for i, r := range bad {
		if _, err := st.RecordLadderReceipt(ctx, "t2", r); !errors.Is(err, domain.ErrInvalidReceipt) {
			t.Errorf("record %d: %v, want ErrInvalidReceipt", i, err)
		}
		// Straight through Execute with a well-formed key: the store itself must
		// refuse before it creates the row, not rely on the caller's check.
		rr := r
		_, err := st.Execute(ctx, domain.LadderActor, domain.CommandID(fmt.Sprintf("direct-%d", i)),
			domain.Command{Kind: domain.CmdRecordLadderReceipt, Mission: "m_ladder-t2", Ladder: &rr})
		if !errors.Is(err, domain.ErrInvalidReceipt) {
			t.Errorf("direct record %d: %v, want ErrInvalidReceipt", i, err)
		}
	}
	for _, q := range []string{"SELECT count(*) FROM missions", "SELECT count(*) FROM events", "SELECT count(*) FROM outbox", "SELECT count(*) FROM commands"} {
		if n := countAll(t, st, q); n != 0 {
			t.Errorf("%s = %d after refused first records", q, n)
		}
	}
}

// NEG-5 / PL-7: the same receipt replays; the same receipt_sha256 with another
// body conflicts, whatever key or path recorded it.
func TestLadderIdempotencyAndShaDedupe(t *testing.T) {
	st, ctx := openTest(t)
	a, err := st.RecordLadderReceipt(ctx, "t3", lfail(1))
	if err != nil || a.Replayed {
		t.Fatalf("first: (%+v, %v)", a, err)
	}
	b, err := st.RecordLadderReceipt(ctx, "t3", lfail(1))
	if err != nil || !b.Replayed || b.LastSeq != a.LastSeq {
		t.Fatalf("replay: (%+v, %v)", b, err)
	}
	changed := lfail(1)
	changed.Class, changed.Fp = "NOT_COUNTED", nil
	if _, err := st.RecordLadderReceipt(ctx, "t3", changed); !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("same receipt_sha256, other body: %v, want ErrIdempotencyConflict", err)
	}
	// Another command id (an import path with its own key): same body replays,
	// a changed body conflicts, and nothing is appended either way.
	r := lfail(1)
	same, err := st.Execute(ctx, domain.LadderActor, "import-key-1", domain.Command{Kind: domain.CmdRecordLadderReceipt, Mission: "m_ladder-t3", Ladder: &r})
	if err != nil || !same.Replayed || same.LastSeq != a.LastSeq {
		t.Fatalf("same receipt under another key: (%+v, %v)", same, err)
	}
	if _, err := st.Execute(ctx, domain.LadderActor, "import-key-2", domain.Command{Kind: domain.CmdRecordLadderReceipt, Mission: "m_ladder-t3", Ladder: &changed}); !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("changed body under another key: %v, want ErrIdempotencyConflict", err)
	}
	if n := countAll(t, st, "SELECT count(*) FROM events WHERE mission_id = 'm_ladder-t3'"); n != 1 {
		t.Fatalf("%d events after replays and conflicts, want 1", n)
	}
}

// PL2-1: two identical add_spend calls both count (1.0), as in Python; a resend
// of the same ordinal is a replay. The broken variant keyed on the body hash
// without the ordinal gives 0.5 and fails this test.
func TestLadderIdenticalSpendsBothCount(t *testing.T) {
	st, ctx := openTest(t)
	if _, err := st.RecordLadderReceipt(ctx, "t4", lspend(500_000, 1)); err != nil {
		t.Fatal(err)
	}
	if r, err := st.RecordLadderReceipt(ctx, "t4", lspend(500_000, 2)); err != nil || r.Replayed {
		t.Fatalf("second identical spend: (%+v, %v)", r, err)
	}
	if r, err := st.RecordLadderReceipt(ctx, "t4", lspend(500_000, 1)); err != nil || !r.Replayed {
		t.Fatalf("resend of ordinal 1: (%+v, %v), want a replay", r, err)
	}
	// The same ordinal with another amount is a conflict, not a third spend.
	if _, err := st.RecordLadderReceipt(ctx, "t4", lspend(600_000, 1)); !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("ordinal 1 reused with another amount: %v, want ErrIdempotencyConflict", err)
	}
	evs, err := st.LadderEvents(ctx, "t4")
	if err != nil {
		t.Fatal(err)
	}
	f, err := ladder.Fold(evs)
	if err != nil {
		t.Fatal(err)
	}
	if f.SpendMicros != 1_000_000 {
		t.Fatalf("spend %d micros, want exactly 1.0 USD from two identical 0.5 spends", f.SpendMicros)
	}
	var twoOnly []domain.Event
	for _, e := range evs {
		if e.Ladder.SpendMicros == 500_000 {
			twoOnly = append(twoOnly, e)
		}
	}
	if len(twoOnly) != 2 {
		t.Fatalf("%d events of 0.5 USD, want exactly 2 (ordinals 1 and 2)", len(twoOnly))
	}
}

// PL2-3 and the cap: only receipts count; cap-1 receipts plus an approval still
// accept the next receipt; the refusal names the cap and stores nothing.
func TestLadderCapCountsReceiptsOnly(t *testing.T) {
	st, ctx := openTest(t)
	const capN = 25 // the production cap is 4096 (TestLadderProductionCap); 4096 transactions would outlive the 60s context
	old := ladderMaxReceipts
	ladderMaxReceipts = capN
	t.Cleanup(func() { ladderMaxReceipts = old })
	for i := 0; i < capN-1; i++ {
		if _, err := st.RecordLadderReceipt(ctx, "t5", lfail(i)); err != nil {
			t.Fatalf("receipt %d: %v", i, err)
		}
	}
	if _, err := st.RecordLadderReceipt(ctx, "t5", lapprove("a_1")); err != nil {
		t.Fatalf("approval at cap-1: %v", err)
	}
	if _, err := st.RecordLadderReceipt(ctx, "t5", lfail(capN-1)); err != nil {
		t.Fatalf("receipt number %d after an approval: %v (approvals must not use receipt slots)", capN, err)
	}
	_, err := st.RecordLadderReceipt(ctx, "t5", lfail(capN))
	if !errors.Is(err, domain.ErrTooManyReceipts) || !strings.Contains(err.Error(), fmt.Sprint(capN)) {
		t.Fatalf("receipt past the cap: %v, want ErrTooManyReceipts naming the cap %d", err, capN)
	}
	if n := countAll(t, st, "SELECT count(*) FROM events WHERE mission_id = 'm_ladder-t5' AND kind = 'LADDER_RECEIPT'"); n != capN {
		t.Fatalf("%d receipts after the refusal, want %d (nothing stored)", n, capN)
	}
	if n := countAll(t, st, "SELECT count(*) FROM commands WHERE command_id = $1", "r"+lhex(1000 + capN)[:62]); n != 0 {
		t.Fatalf("the refused receipt stored %d command rows", n)
	}
	// A log already over the cap (written by another path) is refused to read.
	if _, err := st.pool.Exec(ctx, `INSERT INTO events (mission_id, seq, kind, actor, command_id, payload)
		VALUES ('m_ladder-t5', 1000, 'LADDER_RECEIPT', 'x', 'extra', $1)`,
		[]byte(`{"kind":"LADDER_RECEIPT","mission":"m_ladder-t5","ladder":{"class":"NOT_COUNTED","step":"review","rsha":"`+lrsha(99999)+`"}}`)); err != nil {
		t.Fatal(err)
	}
	if got, err := st.LadderEvents(ctx, "t5"); !errors.Is(err, domain.ErrTooManyReceipts) || got != nil {
		t.Fatalf("over the cap: (%d events, %v), want ErrTooManyReceipts and none", len(got), err)
	}
}

func TestLadderProductionCap(t *testing.T) {
	if ladderMaxReceipts != 4096 || domain.MaxLadderReceipts != 4096 || ladder.MaxReceiptsPerTask != 4096 {
		t.Fatalf("cap drifted: %d %d %d, escalation.py MAX_EVENTS is 4096", ladderMaxReceipts, domain.MaxLadderReceipts, ladder.MaxReceiptsPerTask)
	}
}

func TestLadderApprovalBound(t *testing.T) {
	st, ctx := openTest(t)
	for i := 0; i < domain.MaxLadderApprovals; i++ {
		if _, err := st.RecordLadderReceipt(ctx, "t6", lapprove(fmt.Sprintf("a_%d", i))); err != nil {
			t.Fatalf("approval %d: %v", i, err)
		}
	}
	if _, err := st.RecordLadderReceipt(ctx, "t6", lapprove("a_over")); !errors.Is(err, domain.ErrTooManyReceipts) {
		t.Fatalf("approval past the bound: %v, want ErrTooManyReceipts", err)
	}
}

// D4 ordering: concurrent identical records give one event and no error; many
// first records on a new task race on the row creation and all succeed.
func TestLadderConcurrentRecords(t *testing.T) {
	st, ctx := openTest(t)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := st.RecordLadderReceipt(ctx, "t7", lfail(1))
			errs <- err
		}()
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := st.RecordLadderReceipt(ctx, "t7b", lfail(100+i))
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent record: %v", err)
		}
	}
	if n := countAll(t, st, "SELECT count(*) FROM events WHERE mission_id = 'm_ladder-t7'"); n != 1 {
		t.Fatalf("8 identical concurrent records stored %d events, want 1", n)
	}
	if n := countAll(t, st, "SELECT count(*) FROM events WHERE mission_id = 'm_ladder-t7b'"); n != 8 {
		t.Fatalf("8 distinct concurrent records stored %d events, want 8", n)
	}
	for _, id := range []domain.MissionID{"m_ladder-t7", "m_ladder-t7b"} {
		if _, err := st.Rebuild(ctx, id); err != nil {
			t.Fatalf("rebuild %s: %v", id, err)
		}
	}
}

// AC-1 through the database: after EVERY record the stored log folds to what
// the real Python ledger said, and the final snapshot equals its replay.
func TestLadderOracleThroughTheDatabase(t *testing.T) {
	st, ctx := openTest(t)
	b, err := os.ReadFile("../ladder/testdata/rung_oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	var o struct {
		Seed   int64           `json:"seed"`
		Policy json.RawMessage `json:"policy"`
		Steps  []struct {
			Record domain.LadderRecord `json:"record"`
			Status struct {
				Rung        string   `json:"rung"`
				Fails       int      `json:"failures_at_rung"`
				Climbs      int      `json:"climbs"`
				BlockedBy   []string `json:"blocked_by"`
				Launches    int      `json:"launches"`
				SpendMicros int64    `json:"spend_micros"`
			} `json:"status"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(b, &o); err != nil {
		t.Fatal(err)
	}
	p, err := ladder.DecodePolicy("oracle.json", o.Policy)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("seed=%d", o.Seed)
	n := len(o.Steps)
	if n > 120 {
		n = 120 // the full 272-step prefix comparison is the pure test; this one proves the database path
	}
	if n < 100 {
		t.Fatalf("seed %d: only %d oracle steps", o.Seed, len(o.Steps))
	}
	for i := 0; i < n; i++ {
		s := o.Steps[i]
		if _, err := st.RecordLadderReceipt(ctx, "oracle", s.Record); err != nil {
			t.Fatalf("seed %d step %d: %v", o.Seed, i, err)
		}
		evs, err := st.LadderEvents(ctx, "oracle")
		if err != nil {
			t.Fatal(err)
		}
		got, err := ladder.StatusOf(p, "oracle", evs)
		if err != nil {
			t.Fatal(err)
		}
		if got.Rung != s.Status.Rung || got.FailuresAtRung != s.Status.Fails || got.Climbs != s.Status.Climbs ||
			got.Launches != s.Status.Launches || got.SpendMicros != s.Status.SpendMicros ||
			strings.Join(got.BlockedBy, ",") != strings.Join(s.Status.BlockedBy, ",") {
			t.Fatalf("seed %d step %d: stored log folds to %+v, python said %+v", o.Seed, i, got.LadderStatus, s.Status)
		}
	}
	if _, err := st.Rebuild(ctx, "m_ladder-oracle"); err != nil {
		t.Fatalf("replay digest differs from the snapshot: %v", err)
	}
}

// PL-11: only an IDLE ladder row with no attempts is hidden; a ladder row in a
// bad state is a fault and stays visible.
func TestListMissionsHidesOnlyIdleLadderMissions(t *testing.T) {
	st, ctx := openTest(t)
	if err := st.CreateMission(ctx, "m_real", domain.ProviderClaudeCode, "GoodQA goodqa#1", "", ""); err != nil {
		t.Fatal(err)
	}
	for _, task := range []string{"t8", "t8b"} {
		if _, err := st.RecordLadderReceipt(ctx, task, lclaim("abcdef0")); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := st.ListMissions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != "m_real" {
		t.Fatalf("ListMissions = %+v, want only m_real", rows)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE missions SET execution = 'ADMITTED' WHERE id = 'm_ladder-t8b'`); err != nil {
		t.Fatal(err)
	}
	rows, err = st.ListMissions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	for _, r := range rows {
		seen = append(seen, string(r.ID))
	}
	if strings.Join(seen, ",") != "m_ladder-t8b,m_real" && strings.Join(seen, ",") != "m_real,m_ladder-t8b" {
		t.Fatalf("a forced-ADMITTED ladder row must be listed: %v", seen)
	}
}
