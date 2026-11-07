package sched

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fabric/internal/domain"
)

// goodqa#15082 T6: one transient database error while recording the
// finish must not drop an observed clean exit. The settle is retried
// (it is idempotent by command id) and the run ends VERIFIED, not UNKNOWN.
func TestSettleSurvivesOneTransientFailure(t *testing.T) {
	m := domain.MissionID("m_t6")
	a := domain.AttemptID("r_t6_1")
	st, ln, cfg := ownerSetup(t, m)
	ctx := context.Background()
	resp, err := st.Execute(ctx, "fabd", domain.CommandID("start:"+string(a)), domain.Command{Kind: domain.CmdStartAttempt, Mission: m, Attempt: a, Host: cfg.Host, Role: "writer"})
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
	// The first INSERT of the finish command fails, as a dropped connection
	// would; a sequence is not rolled back, so only the first try fails.
	for _, q := range []string{
		`CREATE SEQUENCE IF NOT EXISTS t6_fail_once`,
		`CREATE OR REPLACE FUNCTION t6_fail_once() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.command_id LIKE 'finish:%' AND nextval('t6_fail_once') = 1 THEN
				RAISE EXCEPTION 'injected transient failure (goodqa#15082 T6)';
			END IF;
			RETURN NEW;
		END $$`,
		`CREATE TRIGGER t6_fail_once BEFORE INSERT ON commands FOR EACH ROW EXECUTE FUNCTION t6_fail_once()`,
	} {
		if _, err := st.Pool().Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		st.Pool().Exec(context.Background(), `DROP TRIGGER IF EXISTS t6_fail_once ON commands`) //nolint:errcheck
		st.Pool().Exec(context.Background(), `DROP FUNCTION IF EXISTS t6_fail_once()`)          //nolint:errcheck
		st.Pool().Exec(context.Background(), `DROP SEQUENCE IF EXISTS t6_fail_once`)            //nolint:errcheck
	})
	s := New(st, cfg, ln)
	s.settleBackoff = 10 * time.Millisecond
	// T6 review N1: the stream changes between tries; the retry must reuse
	// the decided command, not re-derive a conflicting one.
	s.afterSettleTry = func() {
		os.WriteFile(filepath.Join(dir, "stream.jsonl"), []byte(initEv+"\n"+badRes+"\n"), 0o600) //nolint:errcheck
	}
	if err := s.afterWait(ctx, m, a, fence, dir, 0, nil); err != nil {
		t.Errorf("settle gave up: %v", err)
	}
	var tries int64
	st.Pool().QueryRow(ctx, `SELECT last_value FROM t6_fail_once`).Scan(&tries) //nolint:errcheck
	if tries < 2 {
		t.Errorf("the injected failure was not exercised: %d finish inserts", tries)
	}
	var verified int
	st.Pool().QueryRow(ctx, "SELECT count(*) FROM commands WHERE command_id = $1", "verify:"+string(a)).Scan(&verified) //nolint:errcheck
	if got := attemptState(t, st, string(a)); got != string(domain.AttemptVerified) || verified != 1 {
		t.Errorf("after one transient failure: attempt %s, verify commands %d; want VERIFIED, 1", got, verified)
	}
}
