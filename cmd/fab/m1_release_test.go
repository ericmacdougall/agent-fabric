package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fabric/internal/domain"
	"fabric/internal/records"
	"fabric/internal/store"
	"fabric/internal/storetest"
)

// Token-shaped reasons are assembled at run time: this file is part of
// the credential package's clean corpus (corpus2), where a literal would
// count as an exposure.
//
// AC-34 (spec v13): fab mission release refuses a missing, empty, blank or
// multi-line reason, and a missing records location, before it touches the
// store.
func TestMissionReleaseNeedsReasonAndRecords(t *testing.T) {
	for _, args := range [][]string{
		{"--mission", "m_rel", "--work-root", "/w"},
		{"--mission", "m_rel", "--work-root", "/w", "--reason", ""},
		{"--mission", "m_rel", "--work-root", "/w", "--reason", "   "},
		{"--mission", "m_rel", "--work-root", "/w", "--reason", "a\nb"},
		{"--mission", "m_rel", "--work-root", "/w", "--reason", strings.Repeat("x", 201)},
		{"--mission", "bad id", "--work-root", "/w", "--reason", "ok"},
		{"--mission", "m_rel", "--work-root", "/w", "--reason", "caf\u00e9"},
		{"--mission", "m_rel", "--work-root", "/w", "--reason", "token " + strings.Join([]string{"sk", "ant", "oat01", "abcdefgh"}, "-")},
		{"--mission", "m_rel", "--work-root", "/w", "--reason", "dsn postgres://u:p@h/db"},
		{"--mission", "m_rel", "--work-root", "/w", "--reason", "pasted Q7mZp2Xv9LkR4tBn8WcY1HsJ6dFg3NaE5uKoTrVi"},
		{"--mission", "m_rel", "--reason", "ok"},
	} {
		err := missionReleaseMain(args)
		var ue usageError
		if !errors.As(err, &ue) {
			t.Errorf("%q: got %v, want a usage error", args, err)
		}
	}
	if got := releaseRecordsDir("", "", "/srv/w"); got != "/srv/w/run-area/records" {
		t.Fatalf("default records dir %q", got)
	}
	if got := releaseRecordsDir("", "/ra", "/srv/w"); got != "/ra/records" {
		t.Fatalf("run-area records dir %q", got)
	}
}

func releaseFixture(t *testing.T, m domain.MissionID) (context.Context, *store.Store) {
	t.Helper()
	ctx := context.Background()
	st := storetest.Open(t)
	if err := st.CreateMission(ctx, m, domain.ProviderClaudeCode, "GoodQA goodqa#15966", "fabric", "main"); err != nil {
		t.Fatal(err)
	}
	return ctx, st
}

// AC-34, red-team M-3: release is refused, and the hold kept, when the
// records directory is missing or unwritable; a release writes the pending
// record before the delete and the final record after, against the latest
// held attempt; a mission with no hold is an error and records nothing.
func TestReleaseOperatorHoldRecords(t *testing.T) {
	m := domain.MissionID("m_rel")
	ctx, st := releaseFixture(t, m)
	dir := filepath.Join(t.TempDir(), "records")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := releaseOperatorHold(ctx, st, m, "checked", dir, false); err == nil {
		t.Fatal("release without a hold succeeded")
	}
	if rs, _ := records.List(dir, records.ReleaseAuthorized); len(rs) != 0 {
		t.Fatalf("release recorded without a hold: %v", rs)
	}
	if err := st.OperatorHold(ctx, m); err != nil {
		t.Fatal(err)
	}
	if err := records.Append(dir, records.Record{Mission: m, Attempt: "r_rel_1", Kind: records.MissionHold, Class: "redaction_numeric_hit"}); err != nil {
		t.Fatal(err)
	}
	held := func() bool {
		h, err := st.OperatorHeld(ctx, m)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	// A started attempt, so the missing-dir refusal cannot pass for want of
	// an attempt to record against; the ack, so only the directory refuses.
	if _, err := st.Execute(ctx, "chat", "admit-rel", domain.Command{Kind: domain.CmdAdmitMission, Mission: m}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Execute(ctx, "fabd", "start-rel", domain.Command{Kind: domain.CmdStartAttempt, Path: domain.ProviderClaudeCode, Mission: m, Attempt: "r_rel_1", Host: "h_t2", Role: "writer"}); err != nil {
		t.Fatal(err)
	}
	if err := releaseOperatorHold(ctx, st, m, "x", filepath.Join(dir, "missing"), true); err == nil || !held() {
		t.Fatalf("missing records dir: %v, held %v", err, held())
	}
	if os.Geteuid() != 0 {
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		err := releaseOperatorHold(ctx, st, m, "x", dir, true)
		os.Chmod(dir, 0o700) //nolint:errcheck
		if err == nil || !held() {
			t.Fatalf("unwritable records dir: %v, held %v", err, held())
		}
	}
	if err := releaseOperatorHold(ctx, st, m, "rebound the token", dir, false); err != nil {
		t.Fatal(err)
	}
	if held() {
		t.Fatal("still held")
	}
	rs, err := records.List(dir, records.ReleaseAuthorized)
	if err != nil || len(rs) != 1 {
		t.Fatalf("release records %+v %v", rs, err)
	}
	if r := rs[0]; r.Attempt != "r_rel_1" || r.Mission != m || r.Class != releaseClass || r.Generation <= 0 || r.Reason != "rebound the token" || r.AckUnswept {
		t.Fatalf("release record %+v", r)
	}
}

// Spec v15 NEG-35: a hold whose run was never swept (its bound values
// could not be loaded) is released only with --ack-unswept, and the final
// release record carries the ack; a hold with no hold record fails closed
// the same way.
func TestReleaseUnsweptNeedsAck(t *testing.T) {
	for _, class := range []string{"credential_material_unavailable", "credential_rotated_mid_attempt", ""} {
		t.Run(orUnknown(class), func(t *testing.T) {
			m := domain.MissionID("m_relu")
			ctx, st := releaseFixture(t, m)
			dir := filepath.Join(t.TempDir(), "records")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if _, err := st.Execute(ctx, "chat", "admit-relu", domain.Command{Kind: domain.CmdAdmitMission, Mission: m}); err != nil {
				t.Fatal(err)
			}
			attempt := domain.AttemptID("r_relu_1")
			if _, err := st.Execute(ctx, "fabd", "start-relu", domain.Command{Kind: domain.CmdStartAttempt, Path: domain.ProviderClaudeCode, Mission: m, Attempt: attempt, Host: "h_t2", Role: "writer"}); err != nil {
				t.Fatal(err)
			}
			if class != "" {
				if err := records.Append(dir, records.Record{Mission: m, Attempt: attempt, Kind: records.MissionHold, Class: class}); err != nil {
					t.Fatal(err)
				}
			}
			if err := st.OperatorHold(ctx, m); err != nil {
				t.Fatal(err)
			}
			err := releaseOperatorHold(ctx, st, m, "checked", dir, false)
			var ue usageError
			if !errors.As(err, &ue) {
				t.Fatalf("unswept release without the ack: %v", err)
			}
			if held, _ := st.OperatorHeld(ctx, m); !held {
				t.Fatal("the refused release deleted the hold")
			}
			if rs, _ := records.List(dir, records.ReleaseAuthorized); len(rs) != 0 {
				t.Fatalf("a refused release was recorded: %v", rs)
			}
			if err := releaseOperatorHold(ctx, st, m, "files checked by hand", dir, true); err != nil {
				t.Fatal(err)
			}
			rs, _ := records.Read(dir, attempt)
			acked := false
			for _, r := range rs {
				acked = acked || (r.Kind == records.ReleaseAuthorized && r.AckUnswept)
			}
			if !acked {
				t.Fatalf("no acknowledged release record: %+v", rs)
			}
		})
	}
}

// Spec v16 AC-34: the release_authorized record is written durably while
// the row is locked and before the delete. A failure at write, fsync or
// rename rolls back and keeps the hold, with no record; a kill after the
// record and before the commit keeps the hold, and a rerun releases it
// with the same generation recorded once.
func TestReleaseAuthorizationBeforeDelete(t *testing.T) {
	for _, point := range []string{records.FaultWrite, records.FaultSync, records.FaultRename} {
		t.Run(point, func(t *testing.T) {
			m := domain.MissionID("m_relf")
			ctx, st := releaseFixture(t, m)
			dir := filepath.Join(t.TempDir(), "records")
			os.Mkdir(dir, 0o700) //nolint:errcheck
			if err := records.Append(dir, records.Record{Mission: m, Attempt: "r_relf_1", Kind: records.MissionHold, Class: "redaction_numeric_hit"}); err != nil {
				t.Fatal(err)
			}
			if err := st.OperatorHold(ctx, m); err != nil {
				t.Fatal(err)
			}
			records.SetTestFault(func(p string) error {
				if p == point {
					return errors.New("injected " + p)
				}
				return nil
			})
			err := releaseOperatorHold(ctx, st, m, "checked", dir, false)
			records.SetTestFault(nil)
			if err == nil {
				t.Fatal("release succeeded through a failed record write")
			}
			if held, _ := st.OperatorHeld(ctx, m); !held {
				t.Fatal("a failed authorization record released the hold")
			}
			if rs, _ := records.List(dir, records.ReleaseAuthorized); len(rs) != 0 {
				t.Fatalf("a failed write left a record: %+v", rs)
			}
		})
	}
	t.Run("kill before commit", func(t *testing.T) {
		m := domain.MissionID("m_relk")
		ctx, st := releaseFixture(t, m)
		dir := filepath.Join(t.TempDir(), "records")
		os.Mkdir(dir, 0o700) //nolint:errcheck
		if err := records.Append(dir, records.Record{Mission: m, Attempt: "r_relk_1", Kind: records.MissionHold, Class: "redaction_numeric_hit"}); err != nil {
			t.Fatal(err)
		}
		if err := st.OperatorHold(ctx, m); err != nil {
			t.Fatal(err)
		}
		// The release's own steps up to the commit, then the "kill".
		_, err := st.ReleaseOperatorHold(ctx, m, func(g int64) error {
			if err := records.Append(dir, records.Record{Mission: m, Attempt: "r_relk_1", Kind: records.ReleaseAuthorized, Class: releaseClass, Generation: g, Reason: "checked"}); err != nil {
				return err
			}
			return errors.New("killed")
		})
		if err == nil {
			t.Fatal("fixture: the kill did not abort the release")
		}
		if held, _ := st.OperatorHeld(ctx, m); !held {
			t.Fatal("an authorization record alone released the hold")
		}
		if err := releaseOperatorHold(ctx, st, m, "checked", dir, false); err != nil {
			t.Fatal(err)
		}
		if held, _ := st.OperatorHeld(ctx, m); held {
			t.Fatal("the rerun did not release")
		}
		if rs, _ := records.List(dir, records.ReleaseAuthorized); len(rs) != 1 {
			t.Fatalf("%d authorization records, want 1", len(rs))
		}
	})
}
