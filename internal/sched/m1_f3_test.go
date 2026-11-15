package sched

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fabric/internal/credential"
	"fabric/internal/domain"
	"fabric/internal/host"
	"fabric/internal/records"
	"fabric/internal/store"
)

// goodqa#15966 crash-point finding F3: the M1 flag on START lets restart
// recovery find an attempt whose binding record is lost.

// f3Start STARTs a in m as fabd does (with the M1 flag when flag is set),
// optionally writes the binding and ACKs, and returns the fence.
func f3Start(t *testing.T, st *store.Store, cfg Config, bindings string, m domain.MissionID, a domain.AttemptID, flag, bind, ack bool) int64 {
	t.Helper()
	ctx := context.Background()
	cmd := domain.Command{Kind: domain.CmdStartAttempt, Mission: m, Attempt: a, Host: cfg.Host, Role: "writer", Path: domain.ProviderClaudeCode, M1: flag}
	resp, err := st.Execute(ctx, "fabd", domain.CommandID("start:"+string(a)), cmd)
	if err != nil {
		t.Fatal(err)
	}
	fence := resp.Events[0].Fence
	if bind {
		if _, err := cfg.Credential.Policy.PrepareLaunch(bindings, cfg.Credential.IDKey, string(m), string(a), nil); err != nil {
			t.Fatal(err)
		}
	}
	if ack {
		if _, err := st.Execute(ctx, "fabd", domain.CommandID("ack:"+string(a)), domain.Command{Kind: domain.CmdAckStart, Mission: m, Attempt: a, Fence: fence}); err != nil {
			t.Fatal(err)
		}
	}
	return fence
}

// f3Ticks runs n ticks of a fresh (restarted) scheduler and returns its
// stderr, one string per tick.
func f3Ticks(t *testing.T, st *store.Store, cfg Config, ln *fakeLauncher, n int) []string {
	t.Helper()
	verified := 0
	cfg.VerifyGone = func(run domain.AttemptID, _ time.Time) (host.StopReceipt, error) {
		verified++
		return host.StopReceipt{Run: run}, errors.New("unverified")
	}
	s := New(st, cfg, ln)
	s.prober = &fakeProber{alive: false, known: true}
	var out []string
	for i := 0; i < n; i++ {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		saved := os.Stderr
		os.Stderr = w
		done := make(chan string)
		go func() { b, _ := io.ReadAll(r); done <- string(b) }()
		_, terr := s.Tick(context.Background())
		os.Stderr = saved
		w.Close()
		out = append(out, <-done)
		r.Close()
		if terr != nil {
			t.Fatal(terr)
		}
	}
	if verified != 0 {
		t.Fatalf("%d VerifyGone calls; no arm of these tests has a binding and an ACK", verified)
	}
	return out
}

func f3Settled(bindings string, a domain.AttemptID) bool {
	_, err := os.Lstat(filepath.Join(bindings, string(a)+".settled"))
	return err == nil
}

func f3Records(t *testing.T, cfg Config, k records.Kind) []records.Record {
	t.Helper()
	rs, err := records.List(cfg.RecordsDir, k)
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

const f3AlertPrefix = "fabd: ALERT bindings directory lost or emptied;"

// Arm: no binding, with ACK. One lost binding: the
// credential_material_unavailable hold, sweep_incomplete
// (material_unavailable), the settled marker, and no bindings_lost record
// or ALERT line (a single loss is not a lost directory).
func TestM1ScanHoldsLaunchedAttemptWithLostBinding(t *testing.T) {
	const m = domain.MissionID("m_f3a")
	st, ln, cfg, bindings := tokenRig(t, m)
	st.Lease = 300 * time.Millisecond
	const a = domain.AttemptID("r_f3a_1")
	f3Start(t, st, cfg, bindings, m, a, true, true, true)
	if err := os.Remove(filepath.Join(bindings, string(a)+".json")); err != nil {
		t.Fatal(err)
	}
	// I2 before any scan: the lost, unsettled attempt blocks a new START.
	if err := New(st, cfg, ln).checkPriorSettled(context.Background(), m); err == nil || !strings.Contains(err.Error(), classPriorUnsettled) {
		t.Fatalf("checkPriorSettled with a lost binding: %v, want %s", err, classPriorUnsettled)
	}
	waitLeaseLapse(t, st, string(a))
	logs := f3Ticks(t, st, cfg, ln, 3)
	if held, _ := st.OperatorHeld(context.Background(), m); !held {
		t.Fatal("a launched attempt with a lost binding was not held")
	}
	var hold, sweep bool
	for _, r := range f3Records(t, cfg, "") {
		hold = hold || (r.Kind == records.MissionHold && r.Attempt == a && r.Class == credential.ClassMaterialUnavailable)
		sweep = sweep || (r.Kind == records.SweepIncomplete && r.Attempt == a && r.Class == sweepMaterialUnavailable)
	}
	if !hold || !sweep {
		t.Fatalf("records: mission_hold %v, sweep_incomplete material_unavailable %v", hold, sweep)
	}
	if !f3Settled(bindings, a) {
		t.Fatal("not marked settled after its hold committed")
	}
	if rs := f3Records(t, cfg, records.BindingsLost); len(rs) != 0 {
		t.Fatalf("one lost binding wrote %d bindings_lost records", len(rs))
	}
	if strings.Contains(strings.Join(logs, ""), f3AlertPrefix) {
		t.Fatal("one lost binding raised the bindings-lost ALERT")
	}
	// The operator releases; a restarted fabd must not hold it again.
	if ok, err := st.ReleaseOperatorHold(context.Background(), m, func(int64) error { return nil }); err != nil || !ok {
		t.Fatalf("release: %v %v", ok, err)
	}
	f3Ticks(t, st, cfg, ln, 2)
	if held, _ := st.OperatorHeld(context.Background(), m); held {
		t.Fatal("a released lost-binding attempt was held again after a restart")
	}
}

// Arm: no binding, no ACK: settled as unstarted, no hold.
func TestM1ScanSettlesFlaggedStartWithoutBindingOrAck(t *testing.T) {
	const m = domain.MissionID("m_f3b")
	st, ln, cfg, bindings := tokenRig(t, m)
	st.Lease = 300 * time.Millisecond
	const a = domain.AttemptID("r_f3b_1")
	f3Start(t, st, cfg, bindings, m, a, true, false, false)
	waitLeaseLapse(t, st, string(a))
	f3Ticks(t, st, cfg, ln, 3)
	if !f3Settled(bindings, a) {
		t.Fatalf("flagged START with no binding and no ACK not settled (attempt %s)", attemptState(t, st, string(a)))
	}
	if held, _ := st.OperatorHeld(context.Background(), m); held {
		t.Fatal("an attempt that never launched was held")
	}
}

// A pre-M1 START (no flag) with no binding is never visited: no marker, no
// hold, no record.
func TestM1ScanSkipsUnflaggedStartWithoutBinding(t *testing.T) {
	const m = domain.MissionID("m_f3c")
	st, ln, cfg, bindings := tokenRig(t, m)
	st.Lease = 300 * time.Millisecond
	const a = domain.AttemptID("r_f3c_1")
	f3Start(t, st, cfg, bindings, m, a, false, false, true)
	waitLeaseLapse(t, st, string(a))
	f3Ticks(t, st, cfg, ln, 3)
	if f3Settled(bindings, a) {
		t.Fatal("a pre-M1 attempt was visited (settled marker written)")
	}
	if held, _ := st.OperatorHeld(context.Background(), m); held {
		t.Fatal("a pre-M1 attempt was held")
	}
	if rs := f3Records(t, cfg, ""); len(rs) != 0 {
		t.Fatalf("a pre-M1 attempt got %d records", len(rs))
	}
}

// The bindings directory itself is removed: the bindings_lost record is
// written once (counts only), the first scan prints exactly one ALERT line,
// and the lost attempt is held.
func TestM1ScanAlertsOnceWhenBindingsDirLost(t *testing.T) {
	const m = domain.MissionID("m_f3d")
	st, ln, cfg, bindings := tokenRig(t, m)
	st.Lease = 300 * time.Millisecond
	const a = domain.AttemptID("r_f3d_1")
	f3Start(t, st, cfg, bindings, m, a, true, true, true)
	if err := os.RemoveAll(bindings); err != nil {
		t.Fatal(err)
	}
	waitLeaseLapse(t, st, string(a))
	logs := f3Ticks(t, st, cfg, ln, 4)
	total := 0
	for i, l := range logs {
		c := strings.Count(l, f3AlertPrefix)
		if c > 1 {
			t.Fatalf("tick %d printed %d ALERT lines, want at most one per scan", i, c)
		}
		total += c
	}
	if total < 1 {
		t.Fatalf("no ALERT line for a lost bindings directory: %q", logs)
	}
	if !strings.Contains(strings.Join(logs, ""), "1 M1 missions held; see docs/m1-runbook.md#bindings-lost") {
		t.Fatalf("ALERT line lacks the count or the runbook anchor: %q", logs)
	}
	rs := f3Records(t, cfg, records.BindingsLost)
	if len(rs) != 1 || rs[0].Missions != 1 || rs[0].Attempts != 1 {
		t.Fatalf("bindings_lost records %+v, want one with 1 attempt, 1 mission", rs)
	}
	if held, _ := st.OperatorHeld(context.Background(), m); !held {
		t.Fatal("the attempt of a lost bindings directory was not held")
	}
	if fi, err := os.Stat(bindings); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("bindings directory not recreated 0700: %v", err)
	}
	if !f3Settled(bindings, a) {
		t.Fatal("held attempt not marked settled in the recreated directory")
	}
}

// Two lost bindings in one scan (the directory emptied, not removed) also
// raise the alert.
func TestM1ScanAlertsWhenBindingsEmptied(t *testing.T) {
	const m1, m2 = domain.MissionID("m_f3e"), domain.MissionID("m_f3f")
	st, ln, cfg, bindings := tokenRig(t, m1)
	admitSpec(t, st, m2, domain.ProviderClaudeCode, 2)
	st.Lease = 300 * time.Millisecond
	f3Start(t, st, cfg, bindings, m1, "r_f3e_1", true, true, true)
	f3Start(t, st, cfg, bindings, m2, "r_f3f_1", true, true, true)
	ents, _ := os.ReadDir(bindings)
	for _, e := range ents {
		os.Remove(filepath.Join(bindings, e.Name())) //nolint:errcheck
	}
	waitLeaseLapse(t, st, "r_f3e_1")
	waitLeaseLapse(t, st, "r_f3f_1")
	logs := f3Ticks(t, st, cfg, ln, 3)
	if !strings.Contains(strings.Join(logs, ""), f3AlertPrefix+" 2 M1 missions held") {
		t.Fatalf("no ALERT for two lost bindings: %q", logs)
	}
	if rs := f3Records(t, cfg, records.BindingsLost); len(rs) != 1 || rs[0].Missions != 2 {
		t.Fatalf("bindings_lost records %+v, want one with 2 missions", rs)
	}
}

// Production sets the flag: fabd's own START on the token path carries M1,
// and the store reads it back.
func TestM1RunOnceStartCarriesFlag(t *testing.T) {
	const m = domain.MissionID("m_f3g")
	st, ln, cfg, _ := tokenRig(t, m)
	if err := New(st, cfg, ln).runOnce(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	rows, err := st.M1Starts(context.Background(), "", m)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("%d M1-flagged STARTs read back, want fabd's one", len(rows))
	}
}
