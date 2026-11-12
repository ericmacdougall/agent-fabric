package sched

// goodqa#15373 D1: registry dispatch. These tests use a recording launcher
// (no units, no provider) and a real Postgres, like owner_test.go.

import (
	"github.com/jackc/pgx/v5"

	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"fabric/internal/dispatch"
	"fabric/internal/domain"
	"fabric/internal/host"
	"fabric/internal/store"
)

// ioLauncher records Start and StartIO calls. Wait blocks until stop closes.
type ioLauncher struct {
	mu        sync.Mutex
	starts    int
	ioStarts  int
	grants    []host.Grant
	argvs     [][]string
	envs      [][]string
	stdin     []string // what StartIO's stdin held, read by path (never through the fd)
	stdinMode []os.FileMode
	stdinFD   []*os.File
	startErr  error
	waitErr   error
	stop      chan struct{}
}

func newIOLauncher() *ioLauncher { return &ioLauncher{stop: make(chan struct{})} }

func (l *ioLauncher) rec(g host.Grant, argv, env []string) {
	l.grants = append(l.grants, g)
	l.argvs = append(l.argvs, append([]string(nil), argv...))
	l.envs = append(l.envs, append([]string(nil), env...))
}

func (l *ioLauncher) Start(g host.Grant, argv, env []string, stdout, stderr *os.File) (host.Handle, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.starts++
	l.rec(g, argv, env)
	return host.Handle{Run: g.Run}, l.startErr
}

func (l *ioLauncher) StartIO(g host.Grant, argv, env []string, stdin, stdout, stderr *os.File) (host.Handle, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ioStarts++
	l.rec(g, argv, env)
	var b []byte
	var mode os.FileMode
	if stdin != nil {
		b, _ = os.ReadFile(stdin.Name())
		if fi, err := os.Stat(stdin.Name()); err == nil {
			mode = fi.Mode().Perm()
		}
	}
	l.stdin = append(l.stdin, string(b))
	l.stdinMode = append(l.stdinMode, mode)
	l.stdinFD = append(l.stdinFD, stdin)
	return host.Handle{Run: g.Run}, l.startErr
}

func (l *ioLauncher) Wait(host.Handle) (int, error) { <-l.stop; return 0, l.waitErr }

// stopIOLauncher adds a verified Stop (ExitCleanup).
type stopIOLauncher struct {
	*ioLauncher
	stops int
}

func (l *stopIOLauncher) Stop(h host.Handle, _ string, _ time.Duration) (host.StopReceipt, error) {
	l.stops++
	return host.StopReceipt{Run: h.Run, Verified: true}, nil
}

// plainLauncher has no StartIO.
type plainLauncher struct{ ioLauncher }

func (p *plainLauncher) StartIO() {} // not the host.StdinStarter signature

// pinnedProvider writes an executable provider file and returns it with a
// registry whose claude-code dispatcher pins its sha.
func pinnedProvider(t *testing.T) (string, *dispatch.Registry) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fakeprovider")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n# test provider\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sum, err := dispatch.FileSHA256(p)
	if err != nil {
		t.Fatal(err)
	}
	reg := dispatch.NewRegistry()
	if err := reg.Register(dispatch.ClaudeCode{PinSHA256: sum}); err != nil {
		t.Fatal(err)
	}
	return p, reg
}

func registrySetup(t *testing.T, id domain.MissionID) (*store.Store, *ioLauncher, Config) {
	t.Helper()
	st, _, cfg := ownerSetup(t, id)
	ln := newIOLauncher()
	t.Cleanup(func() { close(ln.stop) })
	bin, reg := pinnedProvider(t)
	cfg.Launch.Binary = bin
	cfg.DispatchVia = DispatchRegistry
	cfg.Registry = reg
	cfg.TrustProvider = true
	return st, ln, cfg
}

// AC-2, NEG-12: every refusal is named; the valid configurations pass.
func TestCheckConfig(t *testing.T) {
	_, reg := pinnedProvider(t)
	base := Config{RunnerDSN: "postgres://runner@127.0.0.1:1/x", DispatchVia: DispatchRegistry, Registry: reg, TrustProvider: true, Host: "h_test"}
	ln := newIOLauncher()
	if err := CheckConfig(base, ln); err != nil {
		t.Fatalf("a valid registry config was refused: %v", err)
	}
	// AC-4 gate: a worker account is accepted only once the worker-uid stdin
	// test is recorded (RegistryWorkerUserProven)
	wu := base
	wu.WorkerUser = "fab-worker"
	if err := CheckConfig(wu, ln); RegistryWorkerUserProven != (err == nil) {
		t.Errorf("worker user with RegistryWorkerUserProven=%v: %v", RegistryWorkerUserProven, err)
	} else if err != nil && !strings.Contains(err.Error(), "worker-uid stdin test") {
		t.Errorf("unnamed worker-user refusal: %v", err)
	}
	for _, v := range []string{"", DispatchArgv} {
		c := base
		c.DispatchVia, c.Registry, c.TrustProvider = v, nil, false
		if err := CheckConfig(c, &plainLauncher{}); err != nil {
			t.Errorf("argv config %q refused: %v", v, err)
		}
	}
	cases := map[string]struct {
		mut  func(*Config)
		ln   Launcher
		want string
	}{
		"unknown dispatch-via":      {func(c *Config) { c.DispatchVia = "regsitry" }, ln, "unknown dispatch path"},
		"launcher without StartIO":  {func(c *Config) {}, &plainLauncher{}, "needs a launcher with StartIO"},
		"no claude-code dispatcher": {func(c *Config) { c.Registry = dispatch.NewRegistry() }, ln, `needs a "claude-code" dispatcher`},
		"nil registry":              {func(c *Config) { c.Registry = nil }, ln, `needs a "claude-code" dispatcher`},
		"untrusted provider":        {func(c *Config) { c.TrustProvider = false }, ln, "needs --trust-provider"},
		"negative launch limit":     {func(c *Config) { c.LaunchLimit = -1 }, ln, "negative"},
		"no runner DSN":             {func(c *Config) { c.RunnerDSN = "" }, ln, "runner database role"},
		"exit cleanup without Stop": {func(c *Config) { c.ExitCleanup = true }, ln, "ExitCleanup needs"},
		"host without h_ prefix":    {func(c *Config) { c.Host = "t2-dev" }, ln, "valid --host id"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := base
			c.mut(&cfg)
			err := CheckConfig(cfg, c.ln)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want a refusal naming %q, got %v", c.want, err)
			}
			// New keeps it as the second layer: Tick refuses to run
			if s := New(nil, cfg, c.ln); s.cfgErr == nil {
				t.Fatal("New did not keep the config error")
			}
		})
	}
}

func readGolden(t *testing.T, p string, v any) {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

// AC-5, NEG-2: both paths settle every stream to bd56178's golden command and
// usage.json bytes.
func TestRegistrySettleParity(t *testing.T) {
	_, reg := pinnedProvider(t)
	regD, _ := reg.Get(string(domain.ProviderClaudeCode))
	paths := map[string]dispatch.Dispatcher{"argv": (&Scheduler{}).settler(), "registry": regD}
	cases, err := os.ReadDir("testdata/streams")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, c := range cases {
		if !c.IsDir() || strings.HasPrefix(c.Name(), "_") {
			continue
		}
		n++
		src := filepath.Join("testdata/streams", c.Name())
		var g struct {
			Command *domain.Command `json:"command"`
			Error   *string         `json:"error"`
			Usage   *string         `json:"usage"`
		}
		readGolden(t, filepath.Join(src, "golden.json"), &g)
		eb, _ := os.ReadFile(filepath.Join(src, "exit"))
		code, _ := strconv.Atoi(strings.TrimSpace(string(eb)))
		for path, d := range paths {
			t.Run(c.Name()+"/"+path, func(t *testing.T) {
				run := t.TempDir()
				if fi, _ := os.Stat(filepath.Join(src, "stream.jsonl")); fi != nil && fi.IsDir() {
					os.Mkdir(filepath.Join(run, "stream.jsonl"), 0o755)
				} else {
					b, _ := os.ReadFile(filepath.Join(src, "stream.jsonl"))
					os.WriteFile(filepath.Join(run, "stream.jsonl"), b, 0o644)
				}
				cmd, err := settleCommand(d, "m_golden", "a_golden", 7, run, code)
				if (err != nil) != (g.Error != nil) {
					t.Fatalf("error %v, golden %v", err, g.Error)
				}
				want := g.Command
				if want != nil && strings.HasPrefix(want.Reason, "stream unreadable: ") {
					// the golden embeds bd56178's temp run dir
					w := *want
					w.Reason = "stream unreadable: read " + filepath.Join(run, "stream.jsonl") + ": is a directory"
					want = &w
				}
				if !reflect.DeepEqual(cmd, want) {
					t.Fatalf("command\n got %+v\nwant %+v", cmd, want)
				}
				u, uerr := os.ReadFile(filepath.Join(run, "usage.json"))
				switch {
				case g.Usage == nil && uerr == nil:
					t.Fatalf("usage.json written, golden has none: %s", u)
				case g.Usage != nil && string(u) != *g.Usage:
					t.Fatalf("usage.json\n got %s\nwant %s", u, *g.Usage)
				}
			})
		}
	}
	if n != 13 {
		t.Fatalf("%d golden cases, want 13", n)
	}
}

func argvNormalize(work, token, runnerDSN string, xs []string) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		if runnerDSN != "" {
			x = strings.ReplaceAll(x, runnerDSN, "$RUNNER_DSN")
		}
		x = strings.ReplaceAll(x, token, "$TOKEN")
		out[i] = strings.ReplaceAll(x, work, "$WORK")
	}
	return out
}

// AC-1: with the flag unset or "argv", the launch equals bd56178's.
func TestDispatchViaArgvUnchanged(t *testing.T) {
	var golden struct {
		Argv  []string   `json:"argv"`
		Env   []string   `json:"env"`
		Grant host.Grant `json:"grant"`
	}
	readGolden(t, "testdata/argv/golden.json", &golden)
	for _, via := range []string{"", DispatchArgv} {
		t.Run("via="+via, func(t *testing.T) {
			st, _, cfg := ownerSetup(t, "m_argvg")
			cfg.Launch.Binary = "/bin/true"
			cfg.DispatchVia = via
			ln := newIOLauncher()
			defer close(ln.stop)
			if err := New(st, cfg, ln).runOnce(context.Background(), "m_argvg"); err != nil {
				t.Fatal(err)
			}
			if ln.starts != 1 || ln.ioStarts != 0 {
				t.Fatalf("starts %d, StartIO %d; want 1, 0", ln.starts, ln.ioStarts)
			}
			argv := append([]string(nil), ln.argvs[0]...)
			argv[2] = "$PROMPT"
			g := ln.grants[0]
			tok := g.Token
			g.Token, g.Worktree, g.RunDir = "$TOKEN", strings.ReplaceAll(g.Worktree, cfg.WorkRoot, "$WORK"), strings.ReplaceAll(g.RunDir, cfg.WorkRoot, "$WORK")
			if got := argvNormalize(cfg.WorkRoot, tok, cfg.RunnerDSN, argv); !reflect.DeepEqual(got, golden.Argv) {
				t.Errorf("argv\n got %q\nwant %q", got, golden.Argv)
			}
			if got := argvNormalize(cfg.WorkRoot, tok, cfg.RunnerDSN, ln.envs[0]); !reflect.DeepEqual(got, golden.Env) {
				t.Errorf("env\n got %q\nwant %q", got, golden.Env)
			}
			if !reflect.DeepEqual(g, golden.Grant) {
				t.Errorf("grant\n got %+v\nwant %+v", g, golden.Grant)
			}
			for _, f := range []string{PromptFile, DispatchViaMarker} {
				if _, err := os.Stat(filepath.Join(cfg.WorkRoot, "run", "r_argvg_1", f)); err == nil {
					t.Errorf("the argv path wrote %s", f)
				}
			}
		})
	}
}

// AC-2, AC-3, NEG-4, NEG-5: registry mode launches once through StartIO,
// with the prompt only in a 0600 stdin file and the argv path's env.
func TestRegistryDispatch(t *testing.T) {
	os.Setenv("FABD_TEST_CANARY", "leak") // fabd's own environment must not reach the worker
	defer os.Unsetenv("FABD_TEST_CANARY")
	st, ln, cfg := registrySetup(t, "m_reg1")
	if err := New(st, cfg, ln).runOnce(context.Background(), "m_reg1"); err != nil {
		t.Fatal(err)
	}
	if ln.ioStarts != 1 || ln.starts != 0 {
		t.Fatalf("StartIO %d, Start %d; want 1, 0", ln.ioStarts, ln.starts)
	}
	argv := ln.argvs[0]
	if len(argv) < 4 || argv[1] != "-p" || argv[2] != "--input-format" || argv[3] != "stream-json" {
		t.Fatalf("argv is not the stream-json launch: %q", argv)
	}
	var msg struct {
		Type    string `json:"type"`
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(ln.stdin[0]), &msg); err != nil || msg.Type != "user" || !strings.Contains(msg.Message.Content, `"mission":"m_reg1"`) {
		t.Fatalf("stdin is not this mission's projection: %q (%v)", ln.stdin[0], err)
	}
	for _, a := range argv {
		if strings.Contains(a, msg.Message.Content) || strings.Contains(a, `"checkpoint"`) {
			t.Fatalf("the prompt is in argv: %q", a)
		}
	}
	if ln.stdinMode[0] != 0o600 {
		t.Errorf("prompt.jsonl mode %o, want 600", ln.stdinMode[0])
	}
	run := filepath.Join(cfg.WorkRoot, "run", "r_reg1_1")
	if b, err := os.ReadFile(filepath.Join(run, DispatchViaMarker)); err != nil || string(b) != "registry\n" {
		t.Errorf("dispatch_via marker: %q %v", b, err)
	}
	for _, e := range ln.envs[0] {
		if strings.Contains(e, "FABD_TEST_CANARY") {
			t.Errorf("fabd's environment reached the worker: %s", e)
		}
	}
	// NEG-4: the same env, in order, as the argv path for the same grant
	keys := func(env []string) []string {
		var k []string
		for _, e := range env {
			k = append(k, strings.SplitN(e, "=", 2)[0])
		}
		return k
	}
	var golden struct {
		Env []string `json:"env"`
	}
	readGolden(t, "testdata/argv/golden.json", &golden)
	if !reflect.DeepEqual(keys(ln.envs[0]), keys(golden.Env)) {
		t.Errorf("env keys\n got %q\nwant %q", keys(ln.envs[0]), keys(golden.Env))
	}
}

// NEG-5: the stdin descriptor is closed in the Wait goroutine.
func TestRegistryStdinClosedAfterWait(t *testing.T) {
	st, _, cfg := registrySetup(t, "m_reg2")
	ln := newIOLauncher()
	done := make(chan struct{})
	s := New(st, cfg, ln)
	s.afterWaitDone = func() { close(done) }
	if err := s.runOnce(context.Background(), "m_reg2"); err != nil {
		t.Fatal(err)
	}
	close(ln.stop)
	<-done
	if _, err := ln.stdinFD[0].Stat(); err == nil {
		t.Fatal("the stdin descriptor is still open after Wait")
	}
}

// NEG-6: a prompt file that cannot be created abandons before ACK.
func TestRegistryPromptNotWritten(t *testing.T) {
	st, ln, cfg := registrySetup(t, "m_reg3")
	run := filepath.Join(cfg.WorkRoot, "run", "r_reg3_1")
	os.MkdirAll(run, 0o775)
	os.WriteFile(filepath.Join(run, PromptFile), []byte("stale"), 0o600) // O_EXCL fails
	if err := New(st, cfg, ln).runOnce(context.Background(), "m_reg3"); err != nil {
		t.Fatal(err)
	}
	if ln.ioStarts+ln.starts != 0 {
		t.Fatal("a launch happened without a prompt file")
	}
	var acks, state string
	st.Pool().QueryRow(context.Background(), `SELECT count(*)::text FROM commands WHERE command_id = 'ack:r_reg3_1'`).Scan(&acks) //nolint:errcheck
	st.Pool().QueryRow(context.Background(), `SELECT state FROM attempts WHERE id = 'r_reg3_1'`).Scan(&state)                     //nolint:errcheck
	if acks != "0" || state != "FAILED_DEFINITE" {
		t.Fatalf("ack commands %s, attempt %s; want 0, FAILED_DEFINITE (prelaunch abandon)", acks, state)
	}
}

// NEG-11 (sched side): a provider repointed after start is refused at Plan,
// a prelaunch abandon with no start.
func TestRegistryRepointedProviderAbandons(t *testing.T) {
	st, ln, cfg := registrySetup(t, "m_reg4")
	os.WriteFile(cfg.Launch.Binary, []byte("#!/bin/sh\n# swapped\n"), 0o755)
	if err := New(st, cfg, ln).runOnce(context.Background(), "m_reg4"); err != nil {
		t.Fatal(err)
	}
	var state string
	st.Pool().QueryRow(context.Background(), `SELECT state FROM attempts WHERE id = 'r_reg4_1'`).Scan(&state) //nolint:errcheck
	if ln.ioStarts != 0 || state != "FAILED_DEFINITE" {
		t.Fatalf("StartIO %d, attempt %s; want 0, FAILED_DEFINITE (prelaunch abandon)", ln.ioStarts, state)
	}
}

// NEG-1: an anthropic-api mission is never selected in registry mode.
func TestRegistryAnthropicAPINotSelected(t *testing.T) {
	st, ln, cfg := registrySetup(t, "m_reg5")
	admitSpec(t, st, "m_api1", domain.ProviderAnthropicAPI, 1)
	setCap(t, st, 5)
	s := New(st, cfg, ln)
	for i := 0; i < 3; i++ {
		if _, err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	var api int
	st.Pool().QueryRow(context.Background(), `SELECT count(*) FROM attempts WHERE mission_id = 'm_api1'`).Scan(&api) //nolint:errcheck
	if api != 0 || ln.ioStarts != 1 {
		t.Fatalf("anthropic-api attempts %d, StartIO %d; want 0, 1", api, ln.ioStarts)
	}
}

// AC-7, NEG-3: an ambiguous StartIO is left to lease expiry, never abandoned.
func TestRegistryAmbiguousStart(t *testing.T) {
	st, ln, cfg := registrySetup(t, "m_reg6")
	ln.startErr = errors.New("unit may exist")
	if err := New(st, cfg, ln).runOnce(context.Background(), "m_reg6"); err != nil {
		t.Fatal(err)
	}
	var state string
	st.Pool().QueryRow(context.Background(), `SELECT state FROM attempts WHERE id = 'r_reg6_1'`).Scan(&state) //nolint:errcheck
	if state == "FAILED_DEFINITE" || state == "" {
		t.Fatalf("an ambiguous start ended as %q", state)
	}
	if _, err := os.Stat(filepath.Join(cfg.WorkRoot, "run", "r_reg6_1", PromptFile)); err != nil {
		t.Errorf("the prompt file was not kept for forensics: %v", err)
	}
}

func startCommands(t *testing.T, st *store.Store) map[string]int {
	t.Helper()
	rows, err := st.Pool().Query(context.Background(), `SELECT mission_id FROM commands WHERE command_id LIKE 'start:%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var m string
		rows.Scan(&m) //nolint:errcheck
		out[m]++
	}
	return out
}

// AC-10, NEG-13: the launch limit and the mission filter act before START.
func TestLaunchLimit(t *testing.T) {
	setup := func(t *testing.T) (*store.Store, *ioLauncher, Config) {
		st, ln, cfg := registrySetup(t, "m_lim0")
		setCap(t, st, 10)
		for i := 1; i <= 2; i++ {
			admitSpec(t, st, domain.MissionID(fmt.Sprintf("m_lim%d", i)), domain.ProviderClaudeCode, i)
		}
		return st, ln, cfg
	}
	tick := func(t *testing.T, s *Scheduler) {
		for i := 0; i < 4; i++ {
			if _, err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Run("limit 1 starts one", func(t *testing.T) {
		st, ln, cfg := setup(t)
		cfg.LaunchLimit = 1
		s := New(st, cfg, ln)
		tick(t, s)
		if got := startCommands(t, st); len(got) != 1 || ln.ioStarts != 1 {
			t.Fatalf("START commands %v, StartIO %d; want one", got, ln.ioStarts)
		}
		if p := s.parkedIDs(); len(p) != 0 {
			t.Fatalf("parked %v", p)
		}
	})
	t.Run("an ambiguous start counts", func(t *testing.T) {
		st, ln, cfg := setup(t)
		cfg.LaunchLimit = 1
		ln.startErr = errors.New("unit may exist")
		tick(t, New(st, cfg, ln))
		if got := startCommands(t, st); len(got) != 1 || ln.ioStarts != 1 {
			t.Fatalf("START commands %v, StartIO %d after an ambiguous start; want one", got, ln.ioStarts)
		}
	})
	t.Run("only-mission skips higher priority", func(t *testing.T) {
		st, ln, cfg := setup(t)
		cfg.OnlyMission = "m_lim2"
		tick(t, New(st, cfg, ln))
		if got := startCommands(t, st); len(got) != 1 || got["m_lim2"] != 1 {
			t.Fatalf("START commands %v; want only m_lim2", got)
		}
	})
	t.Run("argv path honours the limit too", func(t *testing.T) {
		st, ln, cfg := setup(t)
		cfg.DispatchVia, cfg.Registry = "", nil
		cfg.Launch.Binary = "/bin/true"
		cfg.LaunchLimit = 2
		tick(t, New(st, cfg, ln))
		if got := startCommands(t, st); len(got) != 2 || ln.starts != 2 {
			t.Fatalf("START commands %v, Start %d; want two", got, ln.starts)
		}
	})
}

// NEG-7: the dispatch_via marker has no reader.
func TestDispatchViaMarkerUnread(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	n := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(f)
		for _, line := range strings.Split(string(b), "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "//") {
				n += strings.Count(line, "DispatchViaMarker")
			}
		}
	}
	// the constant's declaration and its one writer
	if n != 2 {
		t.Fatalf("DispatchViaMarker appears %d times in non-test code; want 2 (declaration, writer)", n)
	}
}

var _ io.Reader = (*os.File)(nil)

// AC-7: a replayed START launches nothing (S1).
func TestRegistryReplay(t *testing.T) {
	st, ln, cfg := registrySetup(t, "m_reg7")
	if _, err := st.Execute(context.Background(), "fabd", "start:r_reg7_1", domain.Command{Kind: domain.CmdStartAttempt, Mission: "m_reg7", Attempt: "r_reg7_1",
		Host: cfg.Host, Role: "writer", Path: domain.ProviderClaudeCode}); err != nil {
		t.Fatal(err)
	}
	if err := New(st, cfg, ln).runOnce(context.Background(), "m_reg7"); err != nil {
		t.Fatal(err)
	}
	if ln.ioStarts+ln.starts != 0 {
		t.Fatalf("a replayed START launched %d workers", ln.ioStarts+ln.starts)
	}
}

// AC-7: a Wait error settles nothing; the attempt is left to lease expiry.
func TestRegistryWaitError(t *testing.T) {
	st, _, cfg := registrySetup(t, "m_reg8")
	ln := newIOLauncher()
	ln.waitErr = errors.New("wait: no child")
	done := make(chan struct{})
	s := New(st, cfg, ln)
	s.afterWaitDone = func() { close(done) }
	if err := s.runOnce(context.Background(), "m_reg8"); err != nil {
		t.Fatal(err)
	}
	writeSuccessfulStream(t, cfg, "r_reg8_1")
	close(ln.stop)
	waitSettlement(t, done)
	if n := settlementCommands(t, st, "r_reg8_1"); n != 0 {
		t.Fatalf("%d settlement commands after a Wait error, want 0", n)
	}
}

// AC-7: with ExitCleanup, the verified Stop runs before the settle.
func TestRegistryExitCleanup(t *testing.T) {
	st, _, cfg := registrySetup(t, "m_reg9")
	cfg.ExitCleanup = true
	ln := &stopIOLauncher{ioLauncher: newIOLauncher()}
	done := make(chan struct{})
	s := New(st, cfg, ln)
	if s.cfgErr != nil {
		t.Fatal(s.cfgErr)
	}
	s.afterWaitDone = func() { close(done) }
	if err := s.runOnce(context.Background(), "m_reg9"); err != nil {
		t.Fatal(err)
	}
	writeSuccessfulStream(t, cfg, "r_reg9_1")
	close(ln.stop)
	waitSettlement(t, done)
	if ln.stops != 1 || settlementCommands(t, st, "r_reg9_1") == 0 {
		t.Fatalf("stops %d, settlement commands %d; want a Stop then a settle", ln.stops, settlementCommands(t, st, "r_reg9_1"))
	}
}

// The live-step gate (fab job dispatch-candidates) runs the dispatch query
// in a READ ONLY transaction and honours --only-mission.
func TestDispatchCandidatesReadOnly(t *testing.T) {
	st, _, _ := registrySetup(t, "m_dc0")
	admitSpec(t, st, "m_dc1", domain.ProviderClaudeCode, 1)
	admitSpec(t, st, "m_dcapi", domain.ProviderAnthropicAPI, 1)
	ctx := context.Background()
	tx, err := st.Pool().BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	all, err := DispatchCandidates(ctx, tx, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(all) != "[m_dc0 m_dc1]" {
		t.Fatalf("candidates %v, want [m_dc0 m_dc1]", all)
	}
	one, err := DispatchCandidates(ctx, tx, nil, "m_dc0")
	if err != nil || fmt.Sprint(one) != "[m_dc0]" {
		t.Fatalf("only-mission candidates %v %v, want [m_dc0]", one, err)
	}
}
