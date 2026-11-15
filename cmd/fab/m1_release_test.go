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
	if rs, _ := records.List(dir, records.MissionRelease); len(rs) != 0 {
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
	rs, err := records.List(dir, records.MissionRelease)
	if err != nil || len(rs) != 2 {
		t.Fatalf("release records %+v %v", rs, err)
	}
	classes := map[string]bool{}
	for _, r := range rs {
		if r.Attempt != "r_rel_1" || r.Mission != m {
			t.Fatalf("release record %+v", r)
		}
		classes[r.Class] = true
	}
	if !classes[releasePending] || !classes[releaseDone] {
		t.Fatalf("release classes %v", classes)
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
			if rs, _ := records.List(dir, records.MissionRelease); len(rs) != 0 {
				t.Fatalf("a refused release was recorded: %v", rs)
			}
			if err := releaseOperatorHold(ctx, st, m, "files checked by hand", dir, true); err != nil {
				t.Fatal(err)
			}
			rs, _ := records.Read(dir, attempt)
			acked := false
			for _, r := range rs {
				acked = acked || (r.Kind == records.MissionRelease && r.Class == releaseAckUnswept)
			}
			if !acked {
				t.Fatalf("no acknowledged release record: %+v", rs)
			}
		})
	}
}
