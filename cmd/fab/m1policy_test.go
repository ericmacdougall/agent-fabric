package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"fabric/internal/domain"
	"fabric/internal/effects"
	"fabric/internal/m1build"
	"fabric/internal/store"
	"fabric/internal/storetest"
)

// goodqa#15966 AC-7(2), AC-32(2), NEG-22, NEG-23: the M1 comment policy is
// enforced for `fab` processes that share the database with fabd, under a
// configuration root that holds no mission config, for M1 and non-M1 builds.

const m1Issue = "7c69d4c8-6ea2-4536-95db-6c2d6555c90d"

// fakeGoodQA is a GoodQA MCP endpoint: it counts every comment_on_issue that
// reaches it (transmissions), keeps the applied comments, and can refuse.
type fakeGoodQA struct {
	mu       sync.Mutex
	comments []map[string]any
	sends    int
	refuse   bool
}

func (f *fakeGoodQA) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"params"`
	}
	json.NewDecoder(r.Body).Decode(&req) //nolint:errcheck
	f.mu.Lock()
	defer f.mu.Unlock()
	var payload any
	isErr := false
	switch req.Params.Name {
	case "get_issue":
		payload = map[string]any{"issue": map[string]any{"id": m1Issue}}
	case "comment_on_issue":
		f.sends++
		if f.refuse {
			isErr = true
			payload = map[string]any{"error": map[string]any{"code": "validation_error"}}
			break
		}
		c := map[string]any{"id": fmt.Sprintf("c%d", len(f.comments)+1), "body": req.Params.Arguments["body"]}
		f.comments = append(f.comments, c)
		payload = map[string]any{"comment": c}
	case "get_issue_comments":
		payload = map[string]any{"comments": f.comments, "hasMore": false}
	}
	text, _ := json.Marshal(payload)
	out, _ := json.Marshal(map[string]any{"result": map[string]any{"isError": isErr, "content": []any{map[string]any{"text": string(text)}}}})
	w.Header().Set("content-type", "application/json")
	w.Write(out) //nolint:errcheck
}

func (f *fakeGoodQA) counts() (sends, comments int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sends, len(f.comments)
}

var (
	m1BinOnce sync.Once
	m1Bins    map[string]string
	m1BinErr  error
)

// fabBinaries builds this package twice: with and without the m1 tag.
func fabBinaries(t *testing.T) map[string]string {
	t.Helper()
	m1BinOnce.Do(func() {
		dir, err := os.MkdirTemp("", "fab-m1-bins-")
		if err != nil {
			m1BinErr = err
			return
		}
		m1Bins = map[string]string{"m1": filepath.Join(dir, "fab-m1"), "non-m1": filepath.Join(dir, "fab")}
		for name, tags := range map[string][]string{"m1": {"-tags", "m1"}, "non-m1": nil} {
			args := append([]string{"build", "-o", m1Bins[name]}, tags...)
			args = append(args, ".")
			cmd := exec.Command("go", args...)
			if out, err := cmd.CombinedOutput(); err != nil {
				m1BinErr = fmt.Errorf("go build %s: %v\n%s", name, err, out)
				return
			}
		}
	})
	if m1BinErr != nil {
		t.Fatal(m1BinErr)
	}
	return m1Bins
}

type m1Env struct {
	ctx   context.Context
	st    *store.Store
	srv   *httptest.Server
	gq    *fakeGoodQA
	key   string // synthetic GoodQA key file path
	root2 string // configuration root R2: no mission config
}

func newM1Env(t *testing.T, marker bool) *m1Env {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	st := storetest.Open(t)
	if err := st.CreateMission(ctx, "m_m1", domain.ProviderClaudeCode, "GoodQA goodqa#15966", "fabric", "main"); err != nil {
		t.Fatal(err)
	}
	for key, cmd := range map[string]domain.Command{
		"admit": {Kind: domain.CmdAdmitMission, Mission: "m_m1"},
	} {
		if _, err := st.Execute(ctx, "fabd", domain.CommandID(key), cmd); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.Execute(ctx, "fabd", "start", domain.Command{Kind: domain.CmdStartAttempt, Path: domain.ProviderClaudeCode,
		Mission: "m_m1", Attempt: "r_m1", Host: "h_t", Role: "writer"}); err != nil {
		t.Fatal(err)
	}
	if marker {
		if _, err := st.Pool().Exec(ctx, `INSERT INTO fabric_settings (key, value) VALUES ('comment_policy', 'm1')
			ON CONFLICT (key) DO UPDATE SET value = 'm1'`); err != nil {
			t.Fatal(err)
		}
	}
	gq := &fakeGoodQA{}
	srv := httptest.NewServer(gq)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	key := filepath.Join(dir, "member.key")
	if err := os.WriteFile(key, []byte("gq_SYNTHETIC_TEST_KEY\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root2 := filepath.Join(dir, "r2")
	os.MkdirAll(root2, 0o700) //nolint:errcheck
	return &m1Env{ctx: ctx, st: st, srv: srv, gq: gq, key: key, root2: root2}
}

func (e *m1Env) executor() *effects.GoodQA {
	return &effects.GoodQA{Endpoint: e.srv.URL, KeyFile: e.key}
}

// fab runs one fab binary as its own OS process under configuration root R2.
func (e *m1Env) fab(t *testing.T, bin string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.CommandContext(e.ctx, bin, args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + e.root2, "FABRIC_HOME=" + e.root2,
		"FABRIC_DSN=" + storetest.DSN, "FABRIC_GOODQA_ENDPOINT=" + e.srv.URL, "FABRIC_GOODQA_KEY_FILE=" + e.key, "FABRIC_ACTOR=op:test"}
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), code
}

func commentArgs(key, body string) []string {
	return []string{"effect", "comment", "--mission", "m_m1", "--attempt", "r_m1", "--fence", "1", "--issue-id", m1Issue,
		"--ticket", "goodqa#15966", "--body", body, "--key", key}
}

func (e *m1Env) rows(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.st.Pool().QueryRow(e.ctx, `SELECT count(*) FROM effects WHERE mission_id = 'm_m1'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func requestJSON(body string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"issue_id": m1Issue, "ticket": "goodqa#15966", "body": body})
	return b
}

// fabdSubmit is fabd's side (configuration root R1, in this process): the
// canonical effect, submitted and dispatched through the broker.
func (e *m1Env) fabdSubmit(t *testing.T, body string) effects.Effect {
	t.Helper()
	b := effects.New(e.st)
	g := e.executor()
	eff, err := b.Submit(e.ctx, g, effects.IntentSpec{Mission: "m_m1", Attempt: "r_m1", Fence: 1, Kind: "ticket_comment",
		Subject: "ticket:goodqa#15966", IdemKey: effects.M1Key("m_m1"), Request: requestJSON(body)})
	if err != nil {
		t.Fatal(err)
	}
	if eff, err = b.Dispatch(e.ctx, g, eff.ID); err != nil {
		t.Fatal(err)
	}
	return eff
}

func TestM1PolicyMultiProcess(t *testing.T) {
	bins := fabBinaries(t)
	canonKey := effects.M1Key("m_m1")
	for _, build := range []string{"m1", "non-m1"} {
		t.Run(build+" fab, fabd confirmed", func(t *testing.T) {
			e := newM1Env(t, true)
			// A different key from R2 is refused before any row is written.
			out, code := e.fab(t, bins[build], commentArgs("k-cli-other", "diagnosis")...)
			if code != 3 || !strings.Contains(out, effects.CodeNonCanonical) {
				t.Fatalf("different key: exit %d\n%s", code, out)
			}
			// AC-7(5): a key that merely looks canonical (prefix) is refused
			// too; the decision is the mission's exact key.
			out, code = e.fab(t, bins[build], commentArgs("m1-000000000000000000000000", "diagnosis")...)
			if code != 3 || !strings.Contains(out, effects.CodeNonCanonical) {
				t.Fatalf("prefix key: exit %d\n%s", code, out)
			}
			if e.rows(t) != 0 {
				t.Fatal("a refused comment wrote a row")
			}
			eff := e.fabdSubmit(t, "diagnosis")
			if eff.State != effects.Confirmed {
				t.Fatalf("fabd effect %s", eff.State)
			}
			// The canonical key from R2 with the same body is a replay.
			out, code = e.fab(t, bins[build], commentArgs(canonKey, "diagnosis")...)
			if code != 0 || !strings.Contains(out, "replayed=true") {
				t.Fatalf("canonical replay: exit %d\n%s", code, out)
			}
			// With another body it is an idempotency conflict, never a row.
			out, code = e.fab(t, bins[build], commentArgs(canonKey, "another body")...)
			if code == 0 || !strings.Contains(strings.ToLower(out), "idempotency") {
				t.Fatalf("canonical, other body: exit %d\n%s", code, out)
			}
			if sends, comments := e.gq.counts(); e.rows(t) != 1 || sends != 1 || comments != 1 {
				t.Fatalf("rows=%d sends=%d comments=%d, want 1/1/1", e.rows(t), sends, comments)
			}
		})
		t.Run(build+" fab, fabd dispatched and refused", func(t *testing.T) {
			// AC-32(2): fabd dispatched; GoodQA refused (FAILED_DEFINITE,
			// dispatches = 1). The CLI's identical replay takes the Retry
			// path and is refused m1_no_redispatch.
			e := newM1Env(t, true)
			e.gq.refuse = true
			if eff := e.fabdSubmit(t, "diagnosis"); eff.State != effects.FailedDefinite || eff.Dispatches != 1 {
				t.Fatalf("fabd effect %s d=%d", eff.State, eff.Dispatches)
			}
			e.gq.mu.Lock()
			e.gq.refuse = false
			e.gq.mu.Unlock()
			out, code := e.fab(t, bins[build], commentArgs(canonKey, "diagnosis")...)
			if code != 3 || !strings.Contains(out, effects.CodeNoRedispatch) {
				t.Fatalf("retry from R2: exit %d\n%s", code, out)
			}
			if sends, _ := e.gq.counts(); sends != 1 || e.rows(t) != 1 {
				t.Fatalf("sends=%d rows=%d, want 1/1", sends, e.rows(t))
			}
		})
	}
	t.Run("m1 fab on an unmarked database", func(t *testing.T) {
		// NEG-23: an M1 build refuses every effect mutation without the
		// marker (exit 3, nothing written); a non-M1 build keeps today's
		// behaviour.
		e := newM1Env(t, false)
		out, code := e.fab(t, bins["m1"], commentArgs(canonKey, "diagnosis")...)
		if code != 3 || !strings.Contains(out, effects.CodeMarkerMissing) {
			t.Fatalf("exit %d\n%s", code, out)
		}
		out, code = e.fab(t, bins["m1"], "effect", "abandon", "--id", "e_000000000000000000000000", "--reason", "r", "--evidence", "x")
		if code != 3 || !strings.Contains(out, effects.CodeMarkerMissing) {
			t.Fatalf("abandon: exit %d\n%s", code, out)
		}
		if e.rows(t) != 0 {
			t.Fatal("rows written")
		}
		out, code = e.fab(t, bins["non-m1"], commentArgs("k-legacy", "legacy")...)
		if code != 0 {
			t.Fatalf("non-m1 unmarked: exit %d\n%s", code, out)
		}
	})
	t.Run("unknown marker", func(t *testing.T) {
		e := newM1Env(t, false)
		e.st.Pool().Exec(e.ctx, `INSERT INTO fabric_settings (key, value) VALUES ('comment_policy', 'm2') ON CONFLICT (key) DO UPDATE SET value = 'm2'`) //nolint:errcheck
		for _, build := range []string{"m1", "non-m1"} {
			out, code := e.fab(t, bins[build], commentArgs(canonKey, "diagnosis")...)
			if code != 3 || !strings.Contains(out, effects.CodePolicyUnknown) {
				t.Fatalf("%s: exit %d\n%s", build, code, out)
			}
		}
	})
}

// goodqa#16039: `fab policy` reports the build and the marker; `fab
// version` stays hex-only; a missing marker exits non-zero and never reads
// as m1.
func TestPolicyCommand(t *testing.T) {
	bins := fabBinaries(t)
	e := newM1Env(t, false)
	if out, code := e.fab(t, bins["m1"], "policy", "build"); code != 0 || strings.TrimSpace(out) != "m1" {
		t.Fatalf("m1 build: %d %q", code, out)
	}
	if out, code := e.fab(t, bins["non-m1"], "policy", "build"); code != 3 || !strings.HasPrefix(out, "non-m1\n") {
		t.Fatalf("non-m1 build: %d %q", code, out)
	}
	out, code := e.fab(t, bins["m1"], "policy", "marker")
	if code == 0 || !strings.HasPrefix(out, "absent\n") {
		t.Fatalf("absent marker: %d %q", code, out)
	}
	e.st.Pool().Exec(e.ctx, `INSERT INTO fabric_settings (key, value) VALUES ('comment_policy', 'm1') ON CONFLICT (key) DO UPDATE SET value = 'm1'`) //nolint:errcheck
	if out, code := e.fab(t, bins["non-m1"], "policy", "marker"); code != 0 || strings.TrimSpace(out) != "m1" {
		t.Fatalf("marker m1: %d %q", code, out)
	}
	for _, b := range bins {
		out, code := e.fab(t, b, "version")
		if code != 0 || strings.Contains(out, "m1") && !isHexLine(out) {
			t.Fatalf("version changed: %d %q", code, out)
		}
	}
}

func isHexLine(s string) bool {
	s = strings.TrimSpace(s)
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return s != ""
}

// `fab db roles --comment-policy m1` is refused by a non-M1 build before
// opening the database.
func TestDBRolesCommentPolicyNeedsM1Build(t *testing.T) {
	err := dbMain([]string{"roles", "--comment-policy", "m1"})
	if m1build.Enabled {
		t.Skip("m1 build")
	}
	if err == nil || exitCode(err) != 3 || !strings.Contains(err.Error(), "M1 build") {
		t.Fatalf("got %v", err)
	}
}

// NEG-23 for fabd (in-process, run with -tags m1): the start check refuses
// an unmarked database with exit 3; daemonMain calls it before any control
// or worker starts.
func TestDaemonPolicyCheck(t *testing.T) {
	e := newM1Env(t, false)
	t.Setenv("FABRIC_DSN", storetest.DSN)
	err := daemonPolicyCheck()
	if !m1build.Enabled {
		if err != nil {
			t.Fatalf("non-m1 build: %v", err)
		}
		return
	}
	if !effects.IsPolicyRefusal(err, effects.CodeMarkerMissing) || exitCode(err) != 3 {
		t.Fatalf("m1 build, no marker: %v", err)
	}
	e.st.Pool().Exec(e.ctx, `INSERT INTO fabric_settings (key, value) VALUES ('comment_policy', 'm1')`) //nolint:errcheck
	if err := daemonPolicyCheck(); err != nil {
		t.Fatalf("m1 build, marker: %v", err)
	}
}

// AC-12: `fab events --mission M` shows the mission's events and its effect
// rows (effect:{state}) ordered by time, without any new event kind.
func TestEventsShowsEffects(t *testing.T) {
	bins := fabBinaries(t)
	e := newM1Env(t, true)
	var kinds0 int
	e.st.Pool().QueryRow(e.ctx, `SELECT count(DISTINCT kind) FROM events WHERE mission_id = 'm_m1'`).Scan(&kinds0) //nolint:errcheck
	e.fabdSubmit(t, "diagnosis")
	out, code := e.fab(t, bins["non-m1"], "events", "--mission", "m_m1")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	idx := map[string]int{}
	for i, l := range lines {
		f := strings.Fields(l)
		if len(f) > 1 {
			if _, ok := idx[f[1]]; !ok {
				idx[f[1]] = i
			}
		}
	}
	for _, want := range []string{"ATTEMPT_STARTED", "effect:INTENT", "effect:DISPATCHED", "effect:CONFIRMED"} {
		if _, ok := idx[want]; !ok {
			t.Fatalf("missing %s in\n%s", want, out)
		}
	}
	if !(idx["ATTEMPT_STARTED"] < idx["effect:INTENT"] && idx["effect:INTENT"] < idx["effect:DISPATCHED"] && idx["effect:DISPATCHED"] < idx["effect:CONFIRMED"]) {
		t.Fatalf("out of order:\n%s", out)
	}
	var kinds1 int
	e.st.Pool().QueryRow(e.ctx, `SELECT count(DISTINCT kind) FROM events WHERE mission_id = 'm_m1'`).Scan(&kinds1) //nolint:errcheck
	if kinds1 != kinds0 {
		t.Fatal("the effect wrote mission events")
	}
}
