package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"fabric/internal/egress"
	"fabric/internal/harness"
	"fabric/internal/harness/claudecode"
)

const claudeDemoBinary = "/Users/eric/.local/share/claude/versions/2.1.321"
const claudeDemoSHA = "abc9feeeca8091d51b623291e044cf0528c2f6b74e0c1d6ef671d52c73f7cd3c"
const claudePolicySHA = "8d15fb8addd7cafd5737385f4044ad6ddc12795d3f81ae60b427160d303e5814"
const claudeHooksSHA = "8db735890b891d316d9767fd16df11b31796a7a6df86ec2a85303d50a032e5b0"

// harnessDemoMain is deliberately separate from the old unconfined test tool.
// Invocation is an operator request, never a waiver of the documented external
// CODE/native-canary/forced-egress/main gates. No guard-clearance file is used.
func harnessDemoMain(args []string) error {
	fs := flag.NewFlagSet("harness demo", flag.ContinueOnError)
	provider := fs.String("provider", "", "claude or codex existing native subscription")
	dir := fs.String("workspace", "", "new empty owned mode-0700 workspace")
	events := fs.String("events", "", "new lifecycle-only canonical JSONL file")
	mc := fs.String("mc-events", "", "optional new lifecycle-only mc.event/1 FileDrop output")
	if err := fs.Parse(args); err != nil {
		return usageErr(err.Error())
	}
	if fs.NArg() != 0 || (*provider != "claude" && *provider != "codex") || *dir == "" || *events == "" {
		return usageErr("usage: fab harness demo --provider claude|codex --workspace DIR --events FILE")
	}
	if runtime.GOOS != "darwin" {
		return errors.New("demo: reviewed Darwin native clients required")
	}
	if err := ownedCodexDir(*dir); err != nil {
		return err
	}
	sink, err := harness.NewJSONLSink(*events)
	if err != nil {
		return err
	}
	var out harness.Sink = sink
	var mcFile *os.File
	if *mc != "" {
		mcFile, err = os.OpenFile(*mc, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			_ = sink.Close()
			return errors.New("demo: lifecycle output unavailable")
		}
		defer mcFile.Close()
		out = &demoLifecycleSink{next: sink, out: mcFile, provider: *provider}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	if *provider == "codex" {
		err = runCodexFileEdit(ctx, codexDemoBinary, *dir, 120*time.Second, out, false, "")
	} else {
		err = runClaudeFixed(ctx, *dir, out)
	}
	if mcFile != nil {
		err = errors.Join(err, mcFile.Sync())
	}
	err = errors.Join(err, sink.Close())
	rows, readErr := harness.ReadJSONL(*events)
	return errors.Join(err, readErr, harness.CheckStream(rows))
}

func startDemoEgress(ctx context.Context, p egress.Provider) (*egress.Mediator, string, error) {
	m, err := egress.Start(ctx, egress.Config{Provider: p, Limits: egress.Limits{Connections: 8, IdleTimeout: 15 * time.Second, TotalTimeout: 120 * time.Second, MaxBytes: 8 << 20}})
	if err != nil {
		return nil, "", errors.New("demo: provider mediator unavailable")
	}
	profile, err := egress.DarwinWorkerProfile(m.Policy())
	if err != nil {
		m.Close()
		return nil, "", err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		m.Close()
		return nil, "", errors.New("demo: log home unavailable")
	}
	profile, err = egress.DarwinPreserveLogs(profile, home)
	if err != nil {
		m.Close()
		return nil, "", err
	}
	return m, profile, nil
}
func mediatedCommand(profile string, argv []string) []string {
	return append([]string{"/usr/bin/sandbox-exec", "-p", profile}, argv...)
}
func addProxyEnv(env map[string]string, m *egress.Mediator) {
	for _, s := range m.Env() {
		k, v, _ := strings.Cut(s, "=")
		env[k] = v
	}
}

func runClaudeFixed(ctx context.Context, dir string, sink harness.Sink) error {
	if err := ownedNonRepository(dir); err != nil {
		return err
	}
	if err := binaryIdentity(claudeDemoBinary, claudeDemoSHA); err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil || home != "/Users/eric" {
		return errors.New("claude: reviewed subscription home required")
	}
	// Local managed sources are not silently excluded. Their appearance requires
	// review of policy and explicit inclusion rather than blanket loader disable.
	for _, p := range []string{"/Library/Application Support/ClaudeCode/CLAUDE.md", "/Library/Application Support/ClaudeCode/managed-settings.json", "/Library/Application Support/ClaudeCode/managed-settings.d", "/Library/Application Support/ClaudeCode/managed-mcp.json", "/Library/Managed Preferences/com.anthropic.claudecode.plist", "/Library/Managed Preferences/eric/com.anthropic.claudecode.plist", filepath.Join(home, "Library/Managed Preferences/com.anthropic.claudecode.plist")} {
		if _, e := os.Lstat(p); !errors.Is(e, os.ErrNotExist) {
			return errors.New("claude: managed input requires separate review")
		}
	}
	policy, settings, err := claudecode.FixedInputSnapshot(filepath.Join(home, ".claude/CLAUDE.md"), filepath.Join(home, ".claude/settings.json"), claudecode.InputIdentity{PolicySHA: claudePolicySHA, HooksSHA: claudeHooksSHA})
	if err != nil {
		return err
	}
	for name, sha := range claudeHookScriptSHA {
		if err := binaryIdentity(filepath.Join(home, ".claude/hooks", name), sha); err != nil {
			return errors.New("claude: mandatory hook script identity changed")
		}
	}
	kit := dir + ".control"
	if err := os.Mkdir(kit, 0700); err != nil {
		return errors.New("claude: owned control directory unavailable")
	}
	// Retain this owned kit on failure for bounded forensic follow-up. It contains
	// current mandatory policy/hooks only, never credentials or old conversations.
	for name, b := range map[string][]byte{"policy.txt": policy, "settings.json": settings, "mcp.json": []byte(`{"mcpServers":{}}`)} {
		f, e := os.OpenFile(filepath.Join(kit, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return errors.New("claude: control snapshot unavailable")
		}
		_, e = f.Write(b)
		e = errors.Join(e, f.Close())
		if e != nil {
			return errors.New("claude: control snapshot failed")
		}
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	nonce[6] = (nonce[6] & 0x0f) | 0x40
	nonce[8] = (nonce[8] & 0x3f) | 0x80
	h := hex.EncodeToString(nonce[:])
	id := h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
	args, err := claudecode.FixedArguments(id, filepath.Join(kit, "settings.json"), filepath.Join(kit, "policy.txt"), filepath.Join(kit, "mcp.json"))
	if err != nil {
		return err
	}
	m, profile, err := startDemoEgress(ctx, egress.ProviderClaude)
	if err != nil {
		return err
	}
	defer m.Close()
	env := claudecode.FixedEnvironment(home)
	addProxyEnv(env, m)
	if err := checkClaudeManagedRuntime(ctx, profile, filepath.Join(kit, "settings.json"), env, dir); err != nil {
		return err
	}
	fixed := &claudecode.FixedWrite{Path: filepath.Join(dir, "demo.txt")}
	a := &claudecode.Adapter{Launch: claudecode.FixedLauncher(mediatedCommand(profile, append([]string{claudeDemoBinary}, args...))), FixedWrite: fixed, SingleTurn: true, StopGrace: 2 * time.Second, DrainGrace: time.Second, DrainHardCap: 3 * time.Second}
	session, err := a.Start(ctx, harness.StartSpec{SessionID: id, Dir: dir, Env: env, Prompt: "Use only Write to create " + fixed.Path + " with exactly fabric followed by one newline. Stop after that single write. Do not read any other file, run commands, or use other tools."}, &demoArtifactSink{dir: dir, next: sink})
	if err != nil {
		return err
	}
	final, err := session.Wait(ctx)
	if err != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return errors.Join(errors.New("claude: interrupted; retain owned state; hook cleanup uncertain"), session.Stop(cleanup))
	}
	if final.Outcome != harness.OutcomeDone || !fixed.Approved() {
		return errors.New("claude: controlled provider job failed; no retry or fallback")
	}
	if err := checkCodexArtifact(dir); err != nil {
		return errors.New("claude: owned artifact verification failed")
	}
	fmt.Printf("Claude owned artifact verified: session=%s artifact=demo.txt\n", id)
	return nil
}

func ownedNonRepository(dir string) error {
	if err := ownedCodexDir(dir); err != nil {
		return err
	}
	for p := dir; ; p = filepath.Dir(p) {
		if _, e := os.Lstat(filepath.Join(p, ".git")); !errors.Is(e, os.ErrNotExist) {
			return errors.New("claude: fresh nonrepository workspace required")
		}
		if p == filepath.Dir(p) {
			break
		}
	}
	return nil
}

func binaryIdentity(path, want string) error {
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() {
		return errors.New("demo: pinned executable unavailable")
	}
	f, err := os.Open(path)
	if err != nil {
		return errors.New("demo: pinned executable unavailable")
	}
	defer f.Close()
	h := sha256.New()
	_, err = io.Copy(h, io.LimitReader(f, 300<<20))
	if err != nil || hex.EncodeToString(h.Sum(nil)) != want {
		return errors.New("demo: executable identity changed")
	}
	return nil
}

// A provider's result cannot publish completed until the owned artifact passes.
type demoArtifactSink struct {
	dir  string
	next harness.Sink
}

func (s *demoArtifactSink) Emit(ctx context.Context, e harness.Event) error {
	if e.Kind == harness.KindFinal {
		var f harness.Final
		if json.Unmarshal(e.Payload, &f) != nil {
			return errors.New("demo: malformed final")
		}
		if f.Outcome == harness.OutcomeDone && checkCodexArtifact(s.dir) != nil {
			f.Outcome = harness.OutcomeFailed
			f.Reason = "owned artifact verification failed"
			e.Payload, _ = json.Marshal(f)
		}
	}
	return s.next.Emit(ctx, e)
}

type demoLifecycleSink struct {
	next              harness.Sink
	out               io.Writer
	provider, session string
	started, final    bool
}

func (s *demoLifecycleSink) Emit(ctx context.Context, e harness.Event) error {
	expected := "claude-code"
	if s.provider == "codex" {
		expected = "codex"
	}
	if e.Harness != expected || s.final || s.session != "" && s.session != e.Session {
		return errors.New("demo: lifecycle identity mismatch")
	}
	if err := s.next.Emit(ctx, e); err != nil {
		return err
	}
	s.session = e.Session
	write := func(seq int, state, severity string) error {
		row := map[string]any{"schema": "mc.event/1", "id": fmt.Sprintf("%s_demo:%s:%d", s.provider, s.session, seq), "ts": time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), "kind": "owner", "source": s.provider + "_demo", "actor": map[string]string{"runtime": s.provider, "session": s.session}, "subject": map[string]string{}, "summary": s.provider + " external provider session " + state, "severity": severity}
		b, _ := json.Marshal(row)
		b = append(b, '\n')
		n, err := s.out.Write(b)
		if err == nil && n != len(b) {
			err = io.ErrShortWrite
		}
		return err
	}
	if !s.started {
		if err := write(1, "started", "info"); err != nil {
			return err
		}
		s.started = true
	}
	if e.Kind == harness.KindFinal {
		var f harness.Final
		if json.Unmarshal(e.Payload, &f) != nil {
			return errors.New("demo: malformed lifecycle final")
		}
		s.final = true
		state, severity := "failed", "high"
		if f.Outcome == harness.OutcomeDone {
			state, severity = "completed", "info"
		}
		return write(2, state, severity)
	}
	return nil
}
