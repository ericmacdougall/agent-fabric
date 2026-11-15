package sched

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"fabric/internal/credential"
	"fabric/internal/credential/sweep"
	"fabric/internal/domain"
	"fabric/internal/host"
	"fabric/internal/records"
	"fabric/internal/store"
)

// quarantineRig is a token-path attempt whose fabd died mid-run: START and
// ACK committed, the binding record durable, raw streams and run roots on
// disk, and the lease lapsed so a restarted fabd sees it UNKNOWN.
type quarantineRig struct {
	st                   *store.Store
	ln                   *fakeLauncher
	cfg                  Config
	mission              domain.MissionID
	attempt              domain.AttemptID
	bindings, sdir, recs string
}

func newQuarantineRig(t *testing.T, m domain.MissionID) *quarantineRig {
	t.Helper()
	ctx := context.Background()
	st, ln, cfg, bindings := tokenRig(t, m)
	st.Lease = 300 * time.Millisecond
	a := domain.AttemptID(fmt.Sprintf("r_%s_1", strings.TrimPrefix(string(m), "m_")))
	resp, err := st.Execute(ctx, "fabd", domain.CommandID("start:"+string(a)), domain.Command{Kind: domain.CmdStartAttempt, Mission: m, Attempt: a,
		Host: cfg.Host, Role: "writer", Path: domain.ProviderClaudeCode})
	if err != nil {
		t.Fatal(err)
	}
	fence := resp.Events[0].Fence
	if _, err := st.Execute(ctx, "fabd", domain.CommandID("ack:"+string(a)), domain.Command{Kind: domain.CmdAckStart, Mission: m, Attempt: a, Fence: fence}); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.Credential.Policy.PrepareLaunch(bindings, cfg.Credential.IDKey, string(m), string(a), nil); err != nil {
		t.Fatal(err)
	}
	r := &quarantineRig{st: st, ln: ln, cfg: cfg, mission: m, attempt: a, bindings: bindings, sdir: filepath.Join(cfg.StreamRoot, string(a)), recs: cfg.RecordsDir}
	run := filepath.Join(cfg.WorkRoot, "run", string(a))
	for _, d := range []string{r.sdir, filepath.Join(run, "claude-config"), filepath.Join(run, "home"), filepath.Join(run, "tmp")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{"stream.jsonl": initEv + "\n" + okResult + "\n", "stderr.log": "warn\n"} {
		if err := os.WriteFile(filepath.Join(r.sdir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	waitLeaseLapse(t, st, string(a))
	return r
}

func (r *quarantineRig) settled() bool {
	_, err := os.Lstat(filepath.Join(r.bindings, string(r.attempt)+".settled"))
	return err == nil
}

func (r *quarantineRig) exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func (r *quarantineRig) kinds(t *testing.T) map[records.Kind]string {
	rs, _ := records.Read(r.recs, r.attempt)
	out := map[records.Kind]string{}
	for _, x := range rs {
		out[x.Kind] = x.Class
	}
	return out
}

// restart runs a fresh fabd's ticks with the runner read back as gone.
func (r *quarantineRig) restart(t *testing.T) *Scheduler {
	t.Helper()
	s := New(r.st, r.cfg, r.ln)
	s.prober = &fakeProber{alive: false, known: true}
	for i := 0; i < 3; i++ {
		if _, err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// goodqa#15966 AC-35(i), E9, NEG-32: after a fabd restart the prober
// quarantines the gone runner. On a verified restart stop the full settle
// sequence runs (redact, gate, sweep, records, MarkSettled, GC) with no
// FINISH and no START. Otherwise the attempt stays quarantined and
// unsettled, sweep_incomplete (restart_unverified) is recorded, the sweep
// material is kept, the raw stream and run roots stay, nothing is swept, and
// the AC-34 hold is placed.
func TestM1RestartQuarantinedSettlesOnVerifiedStop(t *testing.T) {
	for _, verified := range []bool{true, false} {
		name := map[bool]string{true: "verified", false: "unverified"}[verified]
		t.Run(name, func(t *testing.T) {
			r := newQuarantineRig(t, domain.MissionID("m_rq"+name[:1]))
			var checked []domain.AttemptID
			var since time.Time
			r.cfg.VerifyGone = func(run domain.AttemptID, start time.Time) (host.StopReceipt, error) {
				checked, since = append(checked, run), start
				if !verified {
					return host.StopReceipt{Run: run}, errors.New("a worker-uid process outside the manager")
				}
				return host.StopReceipt{Run: run, Verified: true}, nil
			}
			swept := 0
			r.cfg.SweepFor = func(domain.MissionID, domain.AttemptID, string) (sweep.Launcher, error) {
				swept++
				return inProcessSweep, nil
			}
			s := r.restart(t)
			ctx := context.Background()
			if got := attemptState(t, r.st, string(r.attempt)); got != string(domain.AttemptQuarantined) {
				t.Fatalf("attempt %s, want QUARANTINED", got)
			}
			if len(checked) != 1 || checked[0] != r.attempt || since.IsZero() {
				t.Fatalf("VerifyGone calls %v (start %v); want one for %s with its start time", checked, since, r.attempt)
			}
			if n := atomic.LoadInt32(&r.ln.starts); n != 0 {
				t.Fatalf("%d processes started", n)
			}
			held, err := r.st.OperatorHeld(ctx, r.mission)
			if err != nil {
				t.Fatal(err)
			}
			k := r.kinds(t)
			if verified {
				if !r.settled() || swept != 1 || !r.exists(filepath.Join(r.sdir, redactedDone)) || !r.exists(filepath.Join(r.sdir, sweepRecordName)) {
					t.Fatalf("full settle not run: settled=%v swept=%d", r.settled(), swept)
				}
				if r.exists(filepath.Join(r.sdir, "stream.jsonl")) {
					t.Fatal("raw stream kept after a clean settle")
				}
				if _, ok := k[records.SweepIncomplete]; ok || held {
					t.Fatalf("clean recovery recorded %v, held=%v", k, held)
				}
			} else {
				if r.settled() || swept != 0 || r.exists(filepath.Join(r.sdir, redactedDirName)) {
					t.Fatalf("unverified attempt settled=%v swept=%d or redacted", r.settled(), swept)
				}
				if !r.exists(filepath.Join(r.sdir, "stream.jsonl")) || !r.exists(filepath.Join(r.cfg.WorkRoot, "run", string(r.attempt), "home")) {
					t.Fatal("raw stream or run roots removed")
				}
				if !r.exists(filepath.Join(r.bindings, string(r.attempt)+".material")) {
					t.Fatal("no sweep material kept")
				}
				if k[records.SweepIncomplete] != classRestartUnverified || k[records.MissionHold] != classRestartUnverified || !held {
					t.Fatalf("records %v held=%v; want sweep_incomplete and mission_hold %s and the hold", k, held, classRestartUnverified)
				}
				// One pass per process: a later tick neither re-verifies nor
				// re-records.
				if _, err := s.Tick(ctx); err != nil {
					t.Fatal(err)
				}
				if len(checked) != 1 {
					t.Fatalf("VerifyGone called %d times", len(checked))
				}
			}
			var pending int
			r.st.Pool().QueryRow(ctx, `SELECT count(*) FROM attempts WHERE mission_id = $1`, string(r.mission)).Scan(&pending) //nolint:errcheck
			if pending != 1 {
				t.Fatalf("%d attempts; want only the quarantined one", pending)
			}
		})
	}
}

// AC-35(i): a bare `fab reconcile` holds no credential state. A token-path
// attempt it quarantines stays quarantined and unsettled: no settle, no
// FINISH, no START, raw stream and binding kept.
func TestM1BareReconcileKeepsTokenAttemptQuarantined(t *testing.T) {
	r := newQuarantineRig(t, "m_rqb")
	ctx := context.Background()
	res, err := ReconcileHost(ctx, r.st, r.cfg.Host, r.cfg.Host, &fakeProber{alive: false, known: true}, nil)
	if err != nil || res.Quarantined != 1 {
		t.Fatalf("reconcile: %+v %v", res, err)
	}
	if got := attemptState(t, r.st, string(r.attempt)); got != string(domain.AttemptQuarantined) {
		t.Fatalf("attempt %s, want QUARANTINED", got)
	}
	if u, err := credential.Unsettled(r.bindings, string(r.attempt)); err != nil || !u {
		t.Fatalf("binding settled by a bare reconcile (unsettled=%v err=%v)", u, err)
	}
	if !r.exists(filepath.Join(r.sdir, "stream.jsonl")) || r.exists(filepath.Join(r.sdir, redactedDirName)) {
		t.Fatal("a bare reconcile read or removed the raw stream")
	}
	if n := atomic.LoadInt32(&r.ln.starts); n != 0 {
		t.Fatalf("%d processes", n)
	}
}

// goodqa#15966 AC-34, AC-35(i), owner ruling on E: on a verified restart
// stop whose settle owes a hold, the hold commits before MarkSettled. A
// failed hold write leaves the binding unsettled and unheld, and the next
// scan places the hold, with the same class, and then settles. Two hold
// classes: one decided before anything is redacted (the held set changed
// while fabd was down) and one decided after the redaction, sweep and raw
// removal of the first pass (a refused token), so the retry runs on the
// already redacted stream.
func TestM1RecoveryHoldCommitsBeforeSettled(t *testing.T) {
	refused := initEv + "\n" + `{"type":"assistant","session_id":"s1","error":"authentication_failed","message":{"usage":{}}}` + "\n" +
		`{"type":"result","subtype":"success","session_id":"s1","uuid":"r1","num_turns":1,"is_error":true,"result":"token expired"}` + "\n"
	for name, class := range map[string]string{"rotated": credential.ClassRotatedMidAttempt, "refused": "credential_rejected"} {
		t.Run(name, func(t *testing.T) {
			r := newQuarantineRig(t, domain.MissionID("m_rqh"+name[:2]))
			r.cfg.VerifyGone = func(run domain.AttemptID, _ time.Time) (host.StopReceipt, error) {
				return host.StopReceipt{Run: run, Verified: true}, nil
			}
			r.cfg.SweepFor = func(domain.MissionID, domain.AttemptID, string) (sweep.Launcher, error) { return inProcessSweep, nil }
			if name == "rotated" {
				r.cfg.Credential.Others = func() []credential.OtherCredential {
					return []credential.OtherCredential{{Kind: credential.KindGoodQA, Name: "goodqa", Value: []byte("synthetic-goodqa-key-CHANGED-0123456")}}
				}
			} else if err := os.WriteFile(filepath.Join(r.sdir, "stream.jsonl"), []byte(refused), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			s := New(r.st, r.cfg, r.ln)
			s.prober = &fakeProber{alive: false, known: true}
			failed := 0
			s.settleFault = func(step string) error {
				if step == stepOpHold {
					failed++
					return errors.New("injected hold write failure")
				}
				return nil
			}
			for i := 0; i < 3 && failed == 0; i++ {
				if _, err := s.Tick(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if failed == 0 {
				t.Fatal("the recovery never tried to place the hold")
			}
			held, err := r.st.OperatorHeld(ctx, r.mission)
			if err != nil {
				t.Fatal(err)
			}
			if held || r.settled() {
				t.Fatalf("after a failed hold write: held=%v settled=%v; want neither (the marker must wait for the hold)", held, r.settled())
			}
			s.settleFault = nil
			if _, err := s.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			if held, err = r.st.OperatorHeld(ctx, r.mission); err != nil || !held || !r.settled() {
				t.Fatalf("next scan: held=%v settled=%v err=%v; want held, then settled", held, r.settled(), err)
			}
			if k := r.kinds(t); k[records.MissionHold] != class {
				t.Fatalf("records %v; want mission_hold %s", k, class)
			}
			if n := atomic.LoadInt32(&r.ln.starts); n != 0 {
				t.Fatalf("%d processes started", n)
			}
		})
	}
}
