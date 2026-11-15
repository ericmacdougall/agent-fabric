package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"fabric/internal/crashpoint"
	"fabric/internal/credential"
	"fabric/internal/domain"
	"fabric/internal/records"
	"fabric/internal/store"
	"fabric/internal/storetest"
)

// goodqa#15966 crash-point brief section 3, the release half of the
// harness (releaseOperatorHold is in this package; TestCrashPoints in
// internal/sched has the rest). A real child process is killed at every
// release point occurrence, before and after the write, then the parent
// checks I3 (and I2 through it: a mission with no hold is the only one a
// START can reach), retries the release as an operator would, and checks
// again:
//   - I3: the hold row of a generation G is gone only if a durable
//     release_authorized record for G exists.
// The model is a process crash: the release transaction dies with the
// child's connection.

const (
	relEnvScenario = "FABRIC_RELEASE_CRASH_SCENARIO"
	relEnvDir      = "FABRIC_RELEASE_CRASH_DIR"
	relMission     = domain.MissionID("m_relcrash")
	relAttempt     = domain.AttemptID("r_relcrash_1")
)

type relScenario struct {
	name   string
	class  string // the mission_hold record's class
	ack    bool   // --ack-unswept
	rehold bool   // a re-hold races the release
}

var relScenarios = []relScenario{
	{name: "release", class: "credential_rejected"},
	{name: "release-ack-unswept", class: credential.ClassMaterialUnavailable, ack: true},
	{name: "rehold-during-release", class: "credential_rejected", rehold: true},
}

func relAct(ctx context.Context, st *store.Store, dir string, sc relScenario) error {
	if !sc.rehold {
		return releaseOperatorHold(ctx, st, relMission, "checked the run", dir, sc.ack)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(5 * time.Millisecond)
		st.OperatorHold(ctx, relMission) //nolint:errcheck // may land before or after the release
	}()
	err := releaseOperatorHold(ctx, st, relMission, "checked the run", dir, sc.ack)
	wg.Wait()
	return err
}

// TestReleaseCrashChild is the child half; skipped in a normal run.
func TestReleaseCrashChild(t *testing.T) {
	name := os.Getenv(relEnvScenario)
	if name == "" {
		t.Skip("run by TestReleaseCrashPoints only")
	}
	st, err := store.Open(context.Background(), storetest.DSN)
	if err != nil {
		t.Fatal("open the parent's database failed")
	}
	defer st.Close()
	for _, sc := range relScenarios {
		if sc.name == name {
			if err := relAct(context.Background(), st, os.Getenv(relEnvDir), sc); err != nil {
				fmt.Fprintf(os.Stderr, "release child: %v\n", err)
			}
			return
		}
	}
	t.Fatalf("no scenario %s", name)
}

func relGen(t *testing.T, st *store.Store) (int64, bool) {
	t.Helper()
	var g int64
	err := st.Pool().QueryRow(context.Background(), `SELECT last_id FROM outbox_cursors WHERE consumer = $1`, store.OpHoldPrefix+string(relMission)).Scan(&g)
	if err != nil {
		return 0, false
	}
	return g, true
}

// relI3 checks I3 for generation g0, the one held before the release.
func relI3(t *testing.T, st *store.Store, dir string, g0 int64) []string {
	t.Helper()
	g, held := relGen(t, st)
	if held && g == g0 {
		return nil // still held at G: nothing to authorize
	}
	rs, err := records.List(dir, records.ReleaseAuthorized)
	if err != nil {
		return []string{"I3: release records unreadable"}
	}
	for _, r := range rs {
		if r.Generation == g0 && r.Mission == relMission {
			return nil
		}
	}
	return []string{fmt.Sprintf("I3: generation %d released (held=%v) with no release_authorized record for it", g0, held)}
}

func runRelChild(t *testing.T, dir, sc, arm string) (int, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestReleaseCrashChild$", "-test.count=1")
	cmd.Env = append(os.Environ(), storetest.ChildDSNEnv+"="+storetest.DSN, relEnvScenario+"="+sc, relEnvDir+"="+dir, crashpoint.Env+"="+arm)
	var errb strings.Builder
	cmd.Stderr = &errb
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0, errb.String()
	case errors.As(err, &ee):
		return ee.ExitCode(), errb.String()
	}
	t.Fatalf("child did not run: %v", err)
	return 0, ""
}

func TestReleaseCrashPoints(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a child per crash point")
	}
	var report []string
	reached := map[string]bool{}
	fresh := func(t *testing.T, sc relScenario) (*store.Store, string, int64) {
		ctx, st := releaseFixture(t, relMission)
		dir := filepath.Join(t.TempDir(), "records")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := st.OperatorHold(ctx, relMission); err != nil {
			t.Fatal(err)
		}
		if err := records.Append(dir, records.Record{Mission: relMission, Attempt: relAttempt, Kind: records.MissionHold, Class: sc.class}); err != nil {
			t.Fatal(err)
		}
		g0, ok := relGen(t, st)
		if !ok {
			t.Fatal("fixture: no hold")
		}
		return st, dir, g0
	}
	for _, sc := range relScenarios {
		t.Run(sc.name, func(t *testing.T) {
			_, dir, _ := fresh(t, sc)
			code, stderr := runRelChild(t, dir, sc.name, crashpoint.Trace)
			if code != 0 {
				t.Fatalf("trace run exit %d: %s", code, stderr)
			}
			var points []string
			s := bufio.NewScanner(strings.NewReader(stderr))
			for s.Scan() {
				if v, ok := strings.CutPrefix(s.Text(), crashpoint.TraceMarker); ok {
					points = append(points, v)
					reached[strings.SplitN(v, ":", 2)[0]] = true
				}
			}
			if len(points) == 0 {
				t.Fatalf("trace run reached no point: %s", stderr)
			}
			for _, pt := range points {
				t.Run(pt, func(t *testing.T) {
					st, dir, g0 := fresh(t, sc)
					code, stderr := runRelChild(t, dir, sc.name, pt)
					if code != crashpoint.ExitCode || !strings.Contains(stderr, crashpoint.Marker+pt+"\n") {
						t.Errorf("%s at %s: the child did not die there (exit %d)", sc.name, pt, code)
						report = append(report, fmt.Sprintf("| %s | %s | not reached | - |", sc.name, pt))
						return
					}
					v := relI3(t, st, dir, g0)
					// The operator retries; a hold still there must release
					// cleanly, with its own record.
					if g1, held := relGen(t, st); held {
						if err := releaseOperatorHold(context.Background(), st, relMission, "retry after a crash", dir, sc.ack); err != nil {
							v = append(v, "retry: "+err.Error())
						}
						v = append(v, relI3(t, st, dir, g1)...)
					}
					outcome := "ok"
					if len(v) > 0 {
						outcome = "FAIL: " + strings.Join(v, "; ")
						t.Errorf("%s/%s: %s", sc.name, pt, outcome)
					}
					report = append(report, fmt.Sprintf("| %s | %s | died | %s |", sc.name, pt, outcome))
				})
			}
		})
	}
	var missed []string
	for _, p := range crashpoint.Table {
		if p.Path == "release" && !reached[p.Name] {
			missed = append(missed, p.Name)
		}
	}
	if len(missed) > 0 {
		t.Errorf("release points no scenario reaches: %s", strings.Join(missed, ", "))
	}
	sort.Strings(report)
	if out := os.Getenv("FABRIC_CRASH_REPORT"); out != "" {
		body := "| Scenario | Point:pos:n | Child | Invariant I3 |\n|---|---|---|---|\n" + strings.Join(report, "\n") + "\n"
		if err := os.WriteFile(out, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
