package sched

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"fabric/internal/domain"
	"fabric/internal/store"
	"fabric/internal/storetest"
)

// goodqa#15087 increment 3: fabd dispatches claude-code missions only, an
// admitted anthropic-api mission still holds a slot, and the claude-code
// settle uses settle.Finish.

func admitSpec(t *testing.T, st *store.Store, id domain.MissionID, p domain.Provider, prio int) {
	t.Helper()
	ctx := context.Background()
	if err := st.CreateMissionSpec(ctx, store.MissionSpec{ID: id, Ticket: "GoodQA goodqa#15087", Priority: prio, Provider: p}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Execute(ctx, "chat", domain.CommandID("admit-"+string(id)), domain.Command{Kind: domain.CmdAdmitMission, Mission: id}); err != nil {
		t.Fatalf("admit %s: %v", id, err)
	}
}

func setCap(t *testing.T, st *store.Store, n int) {
	t.Helper()
	if _, err := st.Pool().Exec(context.Background(), "UPDATE slots SET cap = $1 WHERE id = 1", n); err != nil {
		t.Fatal(err)
	}
}

// AC-3: 11 anthropic-api missions ahead of one claude-code mission, more
// than the dispatch LIMIT window. The claude-code mission starts; no
// anthropic-api mission starts or is parked in 8 ticks; one dispatch query
// runs per tick.
func TestDispatchOnlyClaudeCodeMissions(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	setCap(t, st, 13)
	for i := 1; i <= 11; i++ {
		admitSpec(t, st, domain.MissionID(fmt.Sprintf("m_api%02d", i)), domain.ProviderAnthropicAPI, i)
	}
	admitSpec(t, st, "m_cc", domain.ProviderClaudeCode, 50)
	if _, err := st.Pool().Exec(ctx, `DELETE FROM sched_counters`); err != nil {
		t.Fatal(err)
	}
	ln := &fakeLauncher{st: st, block: make(chan struct{})}
	t.Cleanup(func() { close(ln.block) })
	cfg := Config{Host: "h_t2", Model: "fake-model", Effort: "n/a", Rung: "R0", TierTableSHA: "test", Profile: "P1", MaxTurns: 5,
		MaxBudgetUSD: 0.01, Wall: time.Minute, WorkRoot: t.TempDir()}
	cfg.Launch.Binary = "/bin/true"
	s := New(st, cfg, ln)
	const ticks = 8
	for i := 0; i < ticks; i++ {
		if _, err := s.Tick(ctx); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
	}
	var apiAttempts, ccAttempts int
	if err := st.Pool().QueryRow(ctx, `SELECT
		(SELECT count(*) FROM attempts a JOIN missions m ON m.id = a.mission_id WHERE m.provider = 'anthropic-api'),
		(SELECT count(*) FROM attempts WHERE mission_id = 'm_cc')`).Scan(&apiAttempts, &ccAttempts); err != nil {
		t.Fatal(err)
	}
	if apiAttempts != 0 || ccAttempts != 1 || ln.starts != 1 {
		t.Fatalf("after %d ticks: %d anthropic-api attempts, %d claude-code attempts, %d launches; want 0, 1, 1", ticks, apiAttempts, ccAttempts, ln.starts)
	}
	if p := s.parkedIDs(); len(p) != 0 {
		t.Fatalf("parked after %d ticks: %v, want none", ticks, p)
	}
	if s.dispatchQueries != ticks {
		t.Fatalf("%d dispatch queries in %d ticks, want one per tick", s.dispatchQueries, ticks)
	}
	rows, err := st.ListMissions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	awaiting := 0
	for _, r := range rows {
		if r.AwaitingGrant {
			awaiting++
			if r.Provider != domain.ProviderAnthropicAPI {
				t.Fatalf("%s (%s) shown as awaiting a grant", r.ID, r.Provider)
			}
		}
	}
	if awaiting != 11 {
		t.Fatalf("%d missions awaiting mission run --grant, want 11", awaiting)
	}
}

// AC-3: at cap 1, an admitted anthropic-api mission holds the slot, so a
// claude-code mission cannot be admitted until it is released.
func TestAnthropicAPIHoldsSlot(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	setCap(t, st, 1)
	admitSpec(t, st, "m_api", domain.ProviderAnthropicAPI, 1)
	if err := st.CreateMissionSpec(ctx, store.MissionSpec{ID: "m_cc", Ticket: "GoodQA goodqa#15087", Priority: 2, Provider: domain.ProviderClaudeCode}); err != nil {
		t.Fatal(err)
	}
	_, err := st.Execute(ctx, "chat", "admit-m_cc", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_cc"})
	if !errors.Is(err, domain.ErrOverCapacity) {
		t.Fatalf("claude-code admission with the slot held by an anthropic-api mission: %v, want over capacity", err)
	}
	if _, err := st.Execute(ctx, "chat", "release-m_api", domain.Command{Kind: domain.CmdRelease, Mission: "m_api"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Execute(ctx, "chat", "admit-m_cc-2", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_cc"}); err != nil {
		t.Fatalf("claude-code admission after the release: %v", err)
	}
}

// AC-7: the claude-code settle goes through settle.Finish. Fault 1 (a
// transient error before commit, SQLSTATE 40001 from a trigger) and fault
// 2 (the finish commits, then the reply is lost) each end in one finish,
// verified; a non-transient error is not retried.
func TestSettleRetryClaudeCode(t *testing.T) {
	ctx := context.Background()
	for _, fault := range []string{"before-commit", "lost-reply", "not-transient"} {
		t.Run(fault, func(t *testing.T) {
			m := domain.MissionID("m_sr")
			a := domain.AttemptID("r_sr_1")
			st, ln, cfg := ownerSetup(t, m)
			resp, err := st.Execute(ctx, "fabd", domain.CommandID("start:"+string(a)), domain.Command{Kind: domain.CmdStartAttempt, Path: domain.ProviderClaudeCode, Mission: m, Attempt: a, Host: cfg.Host, Role: "writer"})
			if err != nil {
				t.Fatal(err)
			}
			fence := resp.Events[0].Fence
			if _, err := st.Execute(ctx, "fabd", domain.CommandID("ack:"+string(a)), domain.Command{Kind: domain.CmdAckStart, Mission: m, Attempt: a, Fence: fence}); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "stream.jsonl"), []byte(initEv+"\n"+okResult+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			code := map[string]string{"before-commit": "40001", "not-transient": "P0001"}[fault]
			if code != "" {
				for _, q := range []string{
					`CREATE SEQUENCE IF NOT EXISTS sr_fail_once`,
					`CREATE OR REPLACE FUNCTION sr_fail_once() RETURNS trigger LANGUAGE plpgsql AS $$
					BEGIN
						IF NEW.command_id LIKE 'finish:%' AND nextval('sr_fail_once') = 1 THEN
							RAISE EXCEPTION 'injected settle failure (goodqa#15087)' USING ERRCODE = '` + code + `';
						END IF;
						RETURN NEW;
					END $$`,
					`CREATE TRIGGER sr_fail_once BEFORE INSERT ON commands FOR EACH ROW EXECUTE FUNCTION sr_fail_once()`,
				} {
					if _, err := st.Pool().Exec(ctx, q); err != nil {
						t.Fatal(err)
					}
				}
				t.Cleanup(func() {
					st.Pool().Exec(context.Background(), `DROP TRIGGER IF EXISTS sr_fail_once ON commands`) //nolint:errcheck
					st.Pool().Exec(context.Background(), `DROP FUNCTION IF EXISTS sr_fail_once()`)          //nolint:errcheck
					st.Pool().Exec(context.Background(), `DROP SEQUENCE IF EXISTS sr_fail_once`)            //nolint:errcheck
				})
			}
			s := New(st, cfg, ln)
			s.settleBackoff = time.Millisecond
			calls := 0
			var replayed []bool
			s.afterFinishExec = func(resp store.Response, err error) error {
				calls++
				replayed = append(replayed, resp.Replayed)
				if fault == "lost-reply" && calls == 1 && err == nil {
					return &pgconn.PgError{Code: "08006", Message: "test: reply lost after commit"}
				}
				return err
			}
			// T6 review N1: the stream changes between tries; the retry must
			// reuse the decided command.
			s.afterSettleTry = func() {
				os.WriteFile(filepath.Join(dir, "stream.jsonl"), []byte(initEv+"\n"+badRes+"\n"), 0o600) //nolint:errcheck
			}
			err = s.afterWait(ctx, m, a, fence, dir, 0, nil)
			var finishCmds, finishEvents int
			st.Pool().QueryRow(ctx, `SELECT (SELECT count(*) FROM commands WHERE command_id = $1),
				(SELECT count(*) FROM events WHERE mission_id = $2 AND kind = 'ATTEMPT_FINISHED')`, "finish:"+string(a), string(m)).Scan(&finishCmds, &finishEvents) //nolint:errcheck
			state := attemptState(t, st, string(a))
			if fault == "not-transient" {
				if err == nil || calls != 1 || finishEvents != 0 {
					t.Fatalf("non-transient error: err %v, %d calls, %d finish events; want the error after 1 call and no finish", err, calls, finishEvents)
				}
				return
			}
			if err != nil || calls != 2 || finishCmds != 1 || finishEvents != 1 || state != string(domain.AttemptVerified) {
				t.Fatalf("%s: err %v, %d calls, %d finish commands, %d finish events, attempt %s; want nil, 2, 1, 1, VERIFIED",
					fault, err, calls, finishCmds, finishEvents, state)
			}
			if fault == "lost-reply" && (replayed[0] || !replayed[1]) {
				t.Fatalf("lost reply: replayed flags %v, want [false true]", replayed)
			}
		})
	}
}
