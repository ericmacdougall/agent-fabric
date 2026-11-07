package native_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"fabric/internal/native"
	"fabric/internal/provider/anthropic/fakeapi"
	"fabric/internal/store"
	"fabric/internal/storetest"
)

// instantClock never waits: the backoff runs on fake time.
type instantClock struct{ now time.Time }

func (c *instantClock) Now() time.Time { return c.now }
func (c *instantClock) After(d time.Duration) <-chan time.Time {
	c.now = c.now.Add(d)
	ch := make(chan time.Time, 1)
	ch <- c.now
	return ch
}

// goodqa#15087 AC-7: finishNative goes through settle.Finish. Fault 1 (a
// transient error before commit, SQLSTATE 40001 from a trigger) and fault
// 2 (the finish commits, then the reply is lost) each end in exactly one
// finish command and one finish event, with the receipt intact.
func TestFinishNativeRetries(t *testing.T) {
	ctx := context.Background()
	for _, fault := range []string{"before-commit", "lost-reply"} {
		t.Run(fault, func(t *testing.T) {
			st := storetest.Open(t)
			e := newEnv(t, fakeapi.Step{Events: fakeapi.Text("finished", 120, 10)})
			if fault == "before-commit" {
				for _, q := range []string{
					`CREATE SEQUENCE IF NOT EXISTS nf_fail_once`,
					`CREATE OR REPLACE FUNCTION nf_fail_once() RETURNS trigger LANGUAGE plpgsql AS $$
					BEGIN
						IF NEW.command_id LIKE 'native-%-finish' AND nextval('nf_fail_once') = 1 THEN
							RAISE EXCEPTION 'injected native finish failure (goodqa#15087)' USING ERRCODE = '40001';
						END IF;
						RETURN NEW;
					END $$`,
					`CREATE TRIGGER nf_fail_once BEFORE INSERT ON commands FOR EACH ROW EXECUTE FUNCTION nf_fail_once()`,
				} {
					if _, err := st.Pool().Exec(ctx, q); err != nil {
						t.Fatal(err)
					}
				}
				t.Cleanup(func() {
					st.Pool().Exec(context.Background(), `DROP TRIGGER IF EXISTS nf_fail_once ON commands`) //nolint:errcheck
					st.Pool().Exec(context.Background(), `DROP FUNCTION IF EXISTS nf_fail_once()`)          //nolint:errcheck
					st.Pool().Exec(context.Background(), `DROP SEQUENCE IF EXISTS nf_fail_once`)            //nolint:errcheck
				})
			}
			calls := 0
			var replayed []bool
			restore := native.SetFinishSeams(func(resp store.Response, err error) error {
				calls++
				replayed = append(replayed, resp.Replayed)
				if fault == "lost-reply" && calls == 1 && err == nil {
					return &pgconn.PgError{Code: "08006", Message: "test: reply lost after commit"}
				}
				return err
			}, &instantClock{now: time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)})
			t.Cleanup(restore)
			r, err := native.Run(ctx, st, e.g, opts(true))
			if err != nil || r.Outcome != "done" {
				t.Fatalf("run with a %s fault: %+v, %v", fault, r, err)
			}
			var cmds, events int
			if err := st.Pool().QueryRow(ctx, `SELECT
				(SELECT count(*) FROM commands WHERE mission_id = 'm_nat1' AND command_id LIKE 'native-%-finish'),
				(SELECT count(*) FROM events WHERE mission_id = 'm_nat1' AND kind = 'ATTEMPT_FINISHED')`).Scan(&cmds, &events); err != nil {
				t.Fatal(err)
			}
			if calls != 2 || cmds != 1 || events != 1 {
				t.Fatalf("%s: %d finish calls, %d finish commands, %d finish events; want 2, 1, 1", fault, calls, cmds, events)
			}
			if fault == "lost-reply" && (replayed[0] || !replayed[1]) {
				t.Fatalf("lost reply: replayed flags %v, want [false true]", replayed)
			}
		})
	}
}
