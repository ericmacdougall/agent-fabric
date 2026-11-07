package native_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/sys/unix"

	"fabric/internal/domain"
	"fabric/internal/hostid"
	"fabric/internal/native"
	"fabric/internal/provider/anthropic/fakeapi"
	"fabric/internal/store"
	"fabric/internal/storetest"
)

// goodqa#15087 increment 4a: the native start rule, the lock file, the
// heartbeat and native T1.

// selfTestMarker wraps the first oracle hook so a test can tell whether
// SelfTest ran: the wrapper touches a marker, then execs the real hook.
func selfTestMarker(t *testing.T, e *env) string {
	t.Helper()
	dir := t.TempDir()
	marker := filepath.Join(dir, "selftest-ran")
	script := filepath.Join(dir, "hook.sh")
	real := e.g.OracleHooks[0]
	body := "#!/bin/sh\ntouch '" + marker + "'\nexec"
	for _, a := range real {
		body += " '" + a + "'"
	}
	if err := os.WriteFile(script, []byte(body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	e.g.OracleHooks = append([][]string{{script}}, e.g.OracleHooks[1:]...)
	return marker
}

// stateDir points the runner's lock files at a per-test directory.
func stateDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	t.Setenv("FABRIC_STATE_DIR", d)
	return d
}

func count(t *testing.T, st *store.Store, q string, args ...any) int {
	t.Helper()
	var n int
	if err := st.Pool().QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// seedRunning starts a native attempt through the ordinary commands, as a
// native run of this host would, and records its agent_runs row (outcome
// "" for none).
func seedRunning(t *testing.T, st *store.Store, e *env, outcome string, runFence int64) int64 {
	t.Helper()
	ctx := context.Background()
	m, a := domain.MissionID(e.g.Mission), domain.AttemptID(e.g.Attempt)
	if err := st.CreateMission(ctx, m, domain.ProviderAnthropicAPI, e.g.Ticket, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Execute(ctx, "chat", domain.CommandID("admit-"+string(m)), domain.Command{Kind: domain.CmdAdmitMission, Mission: m}); err != nil {
		t.Fatal(err)
	}
	lock, err := native.LockPath(a)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := st.Execute(ctx, "native-runner", "seed-start", domain.Command{Kind: domain.CmdStartAttempt, Mission: m, Attempt: a,
		Host: hostid.Default(), Role: "writer", Path: domain.ProviderAnthropicAPI, LockPath: lock})
	if err != nil {
		t.Fatal(err)
	}
	fence := resp.Events[0].Fence
	if _, err := st.Execute(ctx, "native-runner", "seed-ack", domain.Command{Kind: domain.CmdAckStart, Mission: m, Attempt: a, Fence: fence}); err != nil {
		t.Fatal(err)
	}
	if runFence == 0 {
		runFence = fence
	}
	var o *string
	if outcome != "" {
		o = &outcome
	}
	if _, err := st.Pool().Exec(ctx, `INSERT INTO agent_runs (attempt_id, fence, config_sha, billing_mode, outcome, outcome_note)
		VALUES ($1, $2, 'seed', 'api_key', $3, 'seeded')`, string(a), runFence, o); err != nil {
		t.Fatal(err)
	}
	return fence
}

func finishOutcomes(t *testing.T, st *store.Store, m string) []string {
	t.Helper()
	rows, err := st.Pool().Query(context.Background(), `SELECT coalesce(payload->>'outcome', '') FROM events WHERE mission_id = $1 AND kind = 'ATTEMPT_FINISHED' ORDER BY seq`, m)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		rows.Scan(&s) //nolint:errcheck
		out = append(out, s)
	}
	return out
}

func attemptState(t *testing.T, st *store.Store, a string) string {
	t.Helper()
	var s string
	if err := st.Pool().QueryRow(context.Background(), "SELECT state FROM attempts WHERE id = $1", a).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// AC-4, NEG-4: a grant for a claude-code mission, or reusing a claude-code
// attempt id, is refused before SelfTest and the key command, with no
// request, no command and no attempt row.
func TestNativeRefusesClaudeCodeMission(t *testing.T) {
	ctx := context.Background()
	stateDir(t)
	for _, c := range []string{"existing claude-code mission", "claude-code attempt id, other mission", "claude-code attempt id, same mission"} {
		t.Run(c, func(t *testing.T) {
			st := storetest.Open(t)
			e := newEnv(t, fakeapi.Step{Events: fakeapi.Text("never", 1, 1)})
			marker := selfTestMarker(t, e)
			if err := st.CreateMission(ctx, "m_cc", domain.ProviderClaudeCode, "GoodQA goodqa#1", "", ""); err != nil {
				t.Fatal(err)
			}
			switch c {
			case "existing claude-code mission":
				e.g.Mission = "m_cc"
			default:
				if _, err := st.Execute(ctx, "chat", "admit", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_cc"}); err != nil {
					t.Fatal(err)
				}
				if _, err := st.Execute(ctx, "fabd", "start", domain.Command{Kind: domain.CmdStartAttempt, Mission: "m_cc", Attempt: "r_cc",
					Host: "h_t2", Role: "writer", Path: domain.ProviderClaudeCode}); err != nil {
					t.Fatal(err)
				}
				e.g.Attempt = "r_cc"
				if c == "claude-code attempt id, same mission" {
					e.g.Mission = "m_cc"
				}
			}
			cmds := count(t, st, "SELECT count(*) FROM commands")
			attempts := count(t, st, "SELECT count(*) FROM attempts")
			_, err := native.Run(ctx, st, e.g, opts(true))
			if !errors.Is(err, native.ErrStartRefused) {
				t.Fatalf("native run: %v, want ErrStartRefused", err)
			}
			if exists(marker) || exists(e.marker) || len(e.srv.Requests()) != 0 {
				t.Fatalf("selftest ran %v, key read %v, %d requests; want none", exists(marker), exists(e.marker), len(e.srv.Requests()))
			}
			if n := count(t, st, "SELECT count(*) FROM commands"); n != cmds {
				t.Fatalf("commands %d -> %d after a refusal", cmds, n)
			}
			if n := count(t, st, "SELECT count(*) FROM attempts"); n != attempts {
				t.Fatalf("attempts %d -> %d after a refusal", attempts, n)
			}
		})
	}
}

// AC-3: each case of the native start rule.
func TestNativeRunsAdmittedMission(t *testing.T) {
	ctx := context.Background()
	stateDir(t)
	done := func(s string) fakeapi.Step { return fakeapi.Step{Events: fakeapi.Text(s, 50, 5)} }

	t.Run("a operator-admitted runs without a second admit", func(t *testing.T) {
		st := storetest.Open(t)
		e := newEnv(t, done("ok"))
		if err := st.CreateMission(ctx, domain.MissionID(e.g.Mission), domain.ProviderAnthropicAPI, e.g.Ticket, "", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Execute(ctx, "op:test", "admit", domain.Command{Kind: domain.CmdAdmitMission, Mission: domain.MissionID(e.g.Mission)}); err != nil {
			t.Fatal(err)
		}
		held, _ := st.SlotsHeld(ctx)
		r, err := native.Run(ctx, st, e.g, opts(true))
		if err != nil || r.Outcome != "done" {
			t.Fatalf("run: %+v %v", r, err)
		}
		if n := count(t, st, "SELECT count(*) FROM commands WHERE actor = 'native-runner' AND kind = 'ADMIT_MISSION'"); n != 0 {
			t.Fatalf("%d native admits of an admitted mission, want 0", n)
		}
		var host string
		st.Pool().QueryRow(ctx, "SELECT host FROM attempts WHERE id = $1", e.g.Attempt).Scan(&host) //nolint:errcheck
		if domain.HostID(host) != hostid.Default() || host == "h_local" {
			t.Fatalf("attempt host %q, want %s", host, hostid.Default())
		}
		if held < 1 {
			t.Fatalf("the admitted mission held %d slots before the run", held)
		}
	})

	t.Run("b idle mission is admitted and runs", func(t *testing.T) {
		st := storetest.Open(t)
		e := newEnv(t, done("ok"))
		if err := st.CreateMission(ctx, domain.MissionID(e.g.Mission), domain.ProviderAnthropicAPI, e.g.Ticket, "", ""); err != nil {
			t.Fatal(err)
		}
		r, err := native.Run(ctx, st, e.g, opts(true))
		if err != nil || r.Outcome != "done" {
			t.Fatalf("run: %+v %v", r, err)
		}
		if n := count(t, st, "SELECT count(*) FROM commands WHERE actor = 'native-runner' AND kind = 'ADMIT_MISSION'"); n != 1 {
			t.Fatalf("%d native admits, want 1", n)
		}
	})

	t.Run("c replay after a blocked settle sends nothing", func(t *testing.T) {
		st := storetest.Open(t)
		e := newEnv(t, fakeapi.Step{Events: fakeapi.ToolUse("", "", "toolu_1", "read", `{"path":"notes.txt"}`, 50, 5)})
		e.g.MaxTurns = 1
		r, err := native.Run(ctx, st, e.g, opts(true))
		if err != nil || r.Outcome != "max_turns" {
			t.Fatalf("first run: %+v %v, want max_turns", r, err)
		}
		if got := finishOutcomes(t, st, e.g.Mission); len(got) != 1 || got[0] != "blocked" {
			t.Fatalf("finish outcomes %v, want [blocked]", got)
		}
		n := len(e.srv.Requests())
		r, err = native.Run(ctx, st, e.g, opts(true))
		if err != nil || r.Outcome != "max_turns" || len(e.srv.Requests()) != n {
			t.Fatalf("rerun: %+v %v, requests %d -> %d; want the replayed outcome and no request", r, err, n, len(e.srv.Requests()))
		}
		if got := finishOutcomes(t, st, e.g.Mission); len(got) != 1 {
			t.Fatalf("finish events after the replay: %v, want still 1", got)
		}
	})

	t.Run("d crash resume after SIGKILL", func(t *testing.T) {
		st := storetest.Open(t)
		e := newEnv(t,
			fakeapi.Step{Events: fakeapi.ToolUse("", "", "toolu_1", "read", `{"path":"notes.txt"}`, 50, 5)},
			fakeapi.Step{Events: fakeapi.Text("never seen", 50, 5), Stall: 300 * time.Millisecond},
			done("resumed"),
		)
		grant := filepath.Join(t.TempDir(), "grant.json")
		b, _ := json.Marshal(e.g)
		if err := os.WriteFile(grant, b, 0o600); err != nil {
			t.Fatal(err)
		}
		child := exec.Command(os.Args[0], "-test.run=^TestNativeChildHelper$", "-test.count=1")
		child.Env = append(os.Environ(), storetest.ChildDSNEnv+"="+storetest.DSN, "FABRIC_NATIVE_CHILD_GRANT="+grant)
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for len(e.srv.Requests()) < 2 {
			if time.Now().After(deadline) {
				child.Process.Kill() //nolint:errcheck
				child.Wait()         //nolint:errcheck
				t.Fatal("the child never reached its second request")
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err := child.Process.Signal(syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		child.Wait() //nolint:errcheck
		var fence int64
		st.Pool().QueryRow(ctx, "SELECT fence FROM attempts WHERE id = $1", e.g.Attempt).Scan(&fence) //nolint:errcheck
		starts := count(t, st, "SELECT count(*) FROM commands WHERE kind IN ('ADMIT_MISSION', 'START_ATTEMPT')")
		r, err := native.Run(ctx, st, e.g, opts(true))
		if err != nil || r.Outcome != "done" || r.Fence != fence {
			t.Fatalf("resume: %+v %v, want done on fence %d", r, err, fence)
		}
		if n := count(t, st, "SELECT count(*) FROM commands WHERE kind IN ('ADMIT_MISSION', 'START_ATTEMPT')"); n != starts {
			t.Fatalf("admit and start commands %d -> %d on resume, want unchanged", starts, n)
		}
		if got := finishOutcomes(t, st, e.g.Mission); len(got) != 1 || got[0] != "checkpoint" {
			t.Fatalf("finish outcomes %v, want [checkpoint]", got)
		}
	})

	t.Run("e busy lock is refused with no request", func(t *testing.T) {
		st := storetest.Open(t)
		e := newEnv(t, done("never"))
		seedRunning(t, st, e, "", 0)
		lock, _ := native.LockPath(domain.AttemptID(e.g.Attempt))
		if err := os.MkdirAll(filepath.Dir(lock), 0o700); err != nil {
			t.Fatal(err)
		}
		// A second open file description conflicts with the runner's flock
		// exactly as another process would.
		fd, err := unix.Open(lock, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer unix.Close(fd)
		if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
			t.Fatal(err)
		}
		_, err = native.Run(ctx, st, e.g, opts(true))
		if err == nil || !strings.Contains(err.Error(), "attempt busy") || len(e.srv.Requests()) != 0 || exists(e.marker) {
			t.Fatalf("run against a held lock: %v, %d requests, key read %v; want attempt busy, none", err, len(e.srv.Requests()), exists(e.marker))
		}
	})

	t.Run("f refused before the key command", func(t *testing.T) {
		for _, c := range []string{"paused mission", "unknown attempt with an outcome", "fence mismatch", "expired lease"} {
			t.Run(c, func(t *testing.T) {
				st := storetest.Open(t)
				e := newEnv(t, done("never"))
				switch c {
				case "paused mission":
					if err := st.CreateMission(ctx, domain.MissionID(e.g.Mission), domain.ProviderAnthropicAPI, e.g.Ticket, "", ""); err != nil {
						t.Fatal(err)
					}
					if _, err := st.Pool().Exec(ctx, "UPDATE missions SET execution = 'PAUSED' WHERE id = $1", e.g.Mission); err != nil {
						t.Fatal(err)
					}
				case "unknown attempt with an outcome":
					seedRunning(t, st, e, "done", 0)
					st.Pool().Exec(ctx, "UPDATE attempts SET lease_until = now() - interval '1 second' WHERE id = $1", e.g.Attempt) //nolint:errcheck
					if _, err := st.ExpireLeases(ctx, 10); err != nil {
						t.Fatal(err)
					}
					if s := attemptState(t, st, e.g.Attempt); s != string(domain.AttemptUnknown) {
						t.Fatalf("setup: attempt %s, want UNKNOWN", s)
					}
				case "fence mismatch":
					f := seedRunning(t, st, e, "", 0)
					st.Pool().Exec(ctx, "UPDATE agent_runs SET fence = $2 WHERE attempt_id = $1", e.g.Attempt, f+1) //nolint:errcheck
				case "expired lease":
					seedRunning(t, st, e, "", 0)
					st.Pool().Exec(ctx, "UPDATE attempts SET lease_until = now() - interval '1 second' WHERE id = $1", e.g.Attempt) //nolint:errcheck
				}
				cmds := count(t, st, "SELECT count(*) FROM commands")
				_, err := native.Run(ctx, st, e.g, opts(true))
				if !errors.Is(err, native.ErrStartRefused) || exists(e.marker) || len(e.srv.Requests()) != 0 {
					t.Fatalf("%s: %v, key read %v, %d requests; want ErrStartRefused before the key command", c, err, exists(e.marker), len(e.srv.Requests()))
				}
				if n := count(t, st, "SELECT count(*) FROM commands"); n != cmds {
					t.Fatalf("%s: commands %d -> %d", c, cmds, n)
				}
			})
		}
	})
}

// TestNativeChildHelper is the child process of the crash-resume case; it
// runs only when started by that test.
func TestNativeChildHelper(t *testing.T) {
	path := os.Getenv("FABRIC_NATIVE_CHILD_GRANT")
	if path == "" {
		return // started only by TestNativeRunsAdmittedMission; no SKIP line
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var g native.Grant
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), storetest.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	native.Run(context.Background(), st, g, opts(true)) //nolint:errcheck // killed mid-loop
}

// AC-7 native T1.
func TestNativeT1(t *testing.T) {
	ctx := context.Background()
	stateDir(t)
	t.Run("a done with no final turn settles nothing", func(t *testing.T) {
		st := storetest.Open(t)
		e := newEnv(t)
		seedRunning(t, st, e, "done", 0)
		_, err := native.Run(ctx, st, e.g, opts(true))
		if !errors.Is(err, native.ErrNotSettled) || len(finishOutcomes(t, st, e.g.Mission)) != 0 || attemptState(t, st, e.g.Attempt) != "RUNNING" {
			t.Fatalf("%v, finishes %v; want ErrNotSettled, none, RUNNING", err, finishOutcomes(t, st, e.g.Mission))
		}
	})
	t.Run("b unknown outcome settles nothing", func(t *testing.T) {
		st := storetest.Open(t)
		e := newEnv(t)
		seedRunning(t, st, e, "mystery", 0)
		_, err := native.Run(ctx, st, e.g, opts(true))
		if !errors.Is(err, native.ErrNotSettled) || len(finishOutcomes(t, st, e.g.Mission)) != 0 {
			t.Fatalf("%v, finishes %v; want ErrNotSettled, none", err, finishOutcomes(t, st, e.g.Mission))
		}
	})
	t.Run("c done with a final turn is a checkpoint", func(t *testing.T) {
		st := storetest.Open(t)
		e := newEnv(t, fakeapi.Step{Events: fakeapi.Text("finished", 50, 5)})
		if _, err := native.Run(ctx, st, e.g, opts(true)); err != nil {
			t.Fatal(err)
		}
		if got := finishOutcomes(t, st, e.g.Mission); len(got) != 1 || got[0] != "checkpoint" {
			t.Fatalf("finish outcomes %v, want [checkpoint]", got)
		}
	})
	for _, kind := range []string{"parked", "budget_exhausted", "max_turns", "wall_exhausted"} {
		t.Run("d "+kind+" settles blocked", func(t *testing.T) {
			st := storetest.Open(t)
			e := newEnv(t)
			seedRunning(t, st, e, kind, 0)
			if _, err := native.Run(ctx, st, e.g, opts(true)); err != nil {
				t.Fatal(err)
			}
			if got := finishOutcomes(t, st, e.g.Mission); len(got) != 1 || got[0] != "blocked" {
				t.Fatalf("finish outcomes %v, want [blocked]", got)
			}
		})
	}
	t.Run("e a loop error leaves the attempt running", func(t *testing.T) {
		st := storetest.Open(t)
		e := newEnv(t, fakeapi.Step{Status: 400, ErrType: "invalid_request_error", ErrMessage: "test: bad request"})
		if _, err := native.Run(ctx, st, e.g, opts(true)); err == nil {
			t.Fatal("a loop error was reported as success")
		}
		if got := finishOutcomes(t, st, e.g.Mission); len(got) != 0 || attemptState(t, st, e.g.Attempt) != "RUNNING" {
			t.Fatalf("finishes %v, attempt %s; want none, RUNNING", got, attemptState(t, st, e.g.Attempt))
		}
	})
}

// slowTools returns n read steps whose events each stall, so a run lasts
// about n * events * stall of real time.
func slowTools(n int, stall time.Duration) []fakeapi.Step {
	var s []fakeapi.Step
	for i := 0; i < n; i++ {
		s = append(s, fakeapi.Step{Events: fakeapi.ToolUse("", "", fmt.Sprintf("toolu_%d", i), "read", `{"path":"notes.txt"}`, 50, 5), Stall: stall})
	}
	return s
}

// AC-7 heartbeat: a 2 s lease, real time, each case bounded by 10 s.
func TestNativeHeartbeat(t *testing.T) {
	ctx := context.Background()
	stateDir(t)
	t.Run("a a run longer than the lease is not expired", func(t *testing.T) {
		st := storetest.Open(t)
		st.Lease = 2 * time.Second
		e := newEnv(t, append(slowTools(2, 250*time.Millisecond), fakeapi.Step{Events: fakeapi.Text("finished", 50, 5)})...)
		stop := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { // fabd's sweep, every 200 ms
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				case <-time.After(200 * time.Millisecond):
					st.ExpireLeases(ctx, 10) //nolint:errcheck
				}
			}
		}()
		start := time.Now()
		r, err := native.Run(ctx, st, e.g, opts(true))
		close(stop)
		wg.Wait()
		if took := time.Since(start); took < st.Lease {
			t.Fatalf("the run took %s, shorter than the %s lease: the case proves nothing", took, st.Lease)
		}
		if err != nil || r.Outcome != "done" {
			t.Fatalf("run: %+v %v", r, err)
		}
		if n := count(t, st, "SELECT count(*) FROM events WHERE mission_id = $1 AND kind = 'ATTEMPT_UNKNOWN'", e.g.Mission); n != 0 {
			t.Fatalf("%d ATTEMPT_UNKNOWN events during a heartbeating run", n)
		}
	})
	t.Run("b a refused heartbeat stops the loop", func(t *testing.T) {
		st := storetest.Open(t)
		st.Lease = 2 * time.Second
		e := newEnv(t, slowTools(20, 100*time.Millisecond)...)
		var mu sync.Mutex
		atBeat := 0
		restore := native.SetHeartbeatFault(func() error {
			mu.Lock()
			atBeat = len(e.srv.Requests())
			mu.Unlock()
			return nil
		})
		defer restore()
		go func() { // move the attempt to UNKNOWN once the run is under way
			for len(e.srv.Requests()) < 2 {
				time.Sleep(10 * time.Millisecond)
			}
			st.Pool().Exec(ctx, "UPDATE attempts SET lease_until = now() - interval '1 second' WHERE id = $1", e.g.Attempt) //nolint:errcheck
			st.ExpireLeases(ctx, 10)                                                                                        //nolint:errcheck
		}()
		start := time.Now()
		_, err := native.Run(ctx, st, e.g, opts(true))
		if err == nil || !strings.Contains(err.Error(), "heartbeat refused") || time.Since(start) > 10*time.Second {
			t.Fatalf("run after the attempt went UNKNOWN: %v after %s; want heartbeat refused within 10 s", err, time.Since(start))
		}
		mu.Lock()
		last := atBeat
		mu.Unlock()
		if n := len(e.srv.Requests()); n > last+1 {
			t.Fatalf("%d requests, %d at the refused heartbeat: more than the one in flight followed it", n, last)
		}
		if n := count(t, st, "SELECT count(*) FROM commands WHERE kind = 'FINISH_ATTEMPT'"); n != 0 {
			t.Fatalf("%d finish commands after a refused heartbeat, want 0", n)
		}
	})
	t.Run("c heartbeats failing past the lapse stop the loop", func(t *testing.T) {
		st := storetest.Open(t)
		st.Lease = 2 * time.Second
		e := newEnv(t, slowTools(30, 100*time.Millisecond)...)
		restore := native.SetHeartbeatFault(func() error { return &pgconn.PgError{Code: "08006", Message: "test: connection lost"} })
		defer restore()
		start := time.Now()
		_, err := native.Run(ctx, st, e.g, opts(true))
		took := time.Since(start)
		if err == nil || !strings.Contains(err.Error(), "lapsed") || took > 10*time.Second {
			t.Fatalf("run with failing heartbeats: %v after %s; want lapsed within 10 s", err, took)
		}
		if took < st.Lease-st.Lease/3 {
			t.Fatalf("stopped after %s, before the %s lapse", took, st.Lease-st.Lease/3)
		}
		if n := count(t, st, "SELECT count(*) FROM commands WHERE kind = 'FINISH_ATTEMPT'"); n != 0 {
			t.Fatalf("%d finish commands after a lapsed heartbeat, want 0", n)
		}
	})
}
