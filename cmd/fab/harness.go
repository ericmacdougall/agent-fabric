package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"fabric/internal/harness"
	"fabric/internal/harness/claudecode"
	"fabric/internal/native"
	"fabric/internal/store"
)

// harnessMain handles `fab harness ...`. Only `run` exists. The native
// adapter runs a grant; the claude-code adapter runs an interactive session
// scripted from flags and is a TEST TOOL (it is not contained: see
// --unconfined). Other adapters land in their own PRs.
func usageErr(msg string) error { return usageError{errors.New(msg)} }

func harnessMain(args []string) error {
	if len(args) == 0 || args[0] != "run" {
		return usageErr("usage: fab harness run --adapter native|claude-code|codex --events FILE ...")
	}
	return harnessRunMain(args[1:])
}

// slowSink delays every Emit, to prove a stalled sink does not lose a result.
type slowSink struct {
	next  harness.Sink
	delay time.Duration
}

func (s slowSink) Emit(ctx context.Context, e harness.Event) error {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return ctx.Err()
	}
	return s.next.Emit(ctx, e)
}

// passEnvAllowed lists the variables --pass-env may name. They are the
// identity and credential variables a signed-in harness needs; nothing else
// (no FABRIC_*, DSNs, cloud credentials) can reach the child through this flag.
var passEnvAllowed = map[string]bool{
	"USER": true, "LOGNAME": true, "TMPDIR": true, "LANG": true, "LC_ALL": true, "SHELL": true, "TERM": true,
	"ANTHROPIC_API_KEY": true, "CLAUDE_CODE_OAUTH_TOKEN": true,
}

var envNameRE = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

// parsePassEnv validates --pass-env names and returns name=value for the
// ones set in the caller's environment, plus the values that are credentials
// (so they can be masked wherever they would be printed or recorded).
func parsePassEnv(list string) (env map[string]string, secrets []string, err error) {
	env = map[string]string{}
	for _, k := range strings.Split(list, ",") {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		if !envNameRE.MatchString(k) || !passEnvAllowed[k] {
			return nil, nil, usageErr(fmt.Sprintf("--pass-env: %q is not an allowed variable name", k))
		}
		if v, ok := os.LookupEnv(k); ok {
			env[k] = v
			if k == "ANTHROPIC_API_KEY" || k == "CLAUDE_CODE_OAUTH_TOKEN" {
				secrets = append(secrets, v)
			}
		}
	}
	return env, secrets, nil
}

func harnessRunMain(args []string) error {
	fs := flag.NewFlagSet("harness run", flag.ContinueOnError)
	adapter := fs.String("adapter", "", "harness adapter (native, claude-code, codex preflight)")
	grantFile := fs.String("grant", "", "native: grant file")
	events := fs.String("events", "", "new file to write the event trace to (JSONL, mode 0600)")
	fake := fs.Bool("unconfined-fake-host", false, "tests only: run native tools without containment (needs FABRIC_ALLOW_FAKE_HOST=1)")
	slow := fs.Duration("sink-delay", 0, "delay every event write by this long (exercises a slow sink)")
	claudeBin := fs.String("claude-bin", "claude", "claude-code: the claude executable")
	prompt := fs.String("prompt", "", "claude-code: first prompt")
	input := fs.String("input", "", "claude-code: a second message sent after the first")
	cancelInput := fs.Bool("cancel-input", false, "claude-code: also send an Input with an already-cancelled context; it must be refused and not recorded")
	model := fs.String("model", "", "claude-code: model name")
	wait := fs.Duration("wait", 120*time.Second, "claude-code: how long to wait for replies before Stop")
	passEnv := fs.String("pass-env", "", "claude-code: comma-separated variable NAMES to pass in addition to PATH and HOME (allow-listed: USER LOGNAME TMPDIR LANG LC_ALL SHELL TERM ANTHROPIC_API_KEY CLAUDE_CODE_OAUTH_TOKEN)")
	unconfined := fs.Bool("unconfined", false, "claude-code: acknowledge that claude runs with your user's files and no sandbox (a test tool)")
	codexBin := fs.String("codex-bin", codexDemoBinary, "codex: reviewed executable (identity pinned)")
	codexDir := fs.String("codex-dir", "", "codex: new empty mode-0700 workspace")
	codexPreflight := fs.Bool("codex-preflight", false, "codex: verify controls without a model turn")
	codexRun := fs.Bool("codex-run", false, "codex: one restricted fixed file edit (requires reviewed runtime gates)")
	mcEvents := fs.String("mc-events", "", "codex run: optional new lifecycle-only mc.event/1 output")
	if err := fs.Parse(args); err != nil {
		return usageErr(err.Error())
	}
	if fs.NArg() > 0 {
		return usageErr("unexpected arguments: " + strings.Join(fs.Args(), " "))
	}
	if *events == "" {
		return usageErr("--events is required")
	}
	if *adapter != "codex" && (*codexPreflight || *codexDir != "" || *codexBin != codexDemoBinary || *codexRun || *mcEvents != "") {
		return usageErr("--codex-* flags require the codex adapter")
	}
	switch *adapter {
	case "native":
		if *passEnv != "" || *prompt != "" || *input != "" || *cancelInput || *unconfined {
			return usageErr("--pass-env, --prompt, --input, --cancel-input and --unconfined are claude-code flags")
		}
		if *grantFile == "" {
			return usageErr("--grant is required for native")
		}
	case "codex":
		if *codexPreflight == *codexRun {
			return usageErr("choose exactly one of --codex-preflight or --codex-run")
		}
		if *codexPreflight && *mcEvents != "" {
			return usageErr("--mc-events requires --codex-run")
		}
		if *codexDir == "" || *wait <= 0 || *wait > 120*time.Second {
			return usageErr("codex requires --codex-dir and 0 < --wait <= 120s")
		}
		if *grantFile != "" || *fake || *passEnv != "" || *prompt != "" || *input != "" || *cancelInput || *unconfined || *model != "" || *slow != 0 || *claudeBin != "claude" {
			return usageErr("codex preflight rejects other-adapter, prompt, model, environment and sink-delay flags")
		}
	case "claude-code":
		if *prompt == "" {
			return usageErr("--prompt is required for claude-code")
		}
		if !*unconfined {
			return usageErr("claude-code runs with your user's files and no sandbox: pass --unconfined to run this test tool")
		}
		if *grantFile != "" || *fake {
			return usageErr("--grant and --unconfined-fake-host are native flags")
		}
	default:
		return usageErr(fmt.Sprintf("--adapter %q is not wired into fab harness run (native, claude-code)", *adapter))
	}
	extra, secrets, err := parsePassEnv(*passEnv)
	if err != nil {
		return err
	}
	sink, err := harness.NewJSONLSink(*events)
	if err != nil {
		return err
	}
	var out harness.Sink = sink
	if *slow > 0 {
		out = slowSink{next: sink, delay: *slow}
	}
	// SIGINT or SIGTERM is Stop for either adapter.
	sctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	red := harness.ChainRedactor(harness.LiteralRedactor(secrets), harness.PatternRedactor())
	var runErr error
	if *adapter == "codex" {
		runErr = runCodexFileEdit(sctx, *codexBin, *codexDir, *wait, out, *codexPreflight, *mcEvents)
	} else if *adapter == "native" {
		runErr = runNative(sctx, *grantFile, *fake, out)
	} else {
		runErr = runClaudeCode(sctx, *claudeBin, *model, *prompt, *input, extra, red, *cancelInput, *wait, out)
	}
	if cerr := sink.Close(); cerr != nil && runErr == nil {
		runErr = cerr
	}
	evs, rerr := harness.ReadJSONL(*events)
	if rerr != nil {
		return errors.Join(runErr, rerr)
	}
	if *adapter == "codex" {
		return errors.Join(runErr, harness.CheckStream(evs))
	}
	return errors.Join(runErr, judge(*adapter, evs))
}

func runNative(sctx context.Context, grantFile string, fake bool, sink harness.Sink) error {
	g, err := native.LoadGrant(grantFile)
	if err != nil {
		return err
	}
	opts, err := nativeOptions(g, fake)
	if err != nil {
		return err
	}
	defer opts.Close()
	opts.Sink = sink
	return withStore(func(_ context.Context, st *store.Store) error {
		ctx, cancel := context.WithTimeout(sctx, time.Duration(g.WallSeconds+120)*time.Second)
		defer cancel()
		r, err := native.Run(ctx, st, g, opts)
		if r != nil {
			b, _ := json.MarshalIndent(r, "", "  ")
			fmt.Println(string(b))
		}
		return err
	})
}

// runClaudeCode drives one interactive Claude Code session: the prompt, an
// Input with an already-cancelled context (it must be refused), a real second
// Input, an Interrupt, and a Stop. The child's environment is PATH, HOME and
// the allow-listed variables named by --pass-env.
func runClaudeCode(sctx context.Context, bin, model, prompt, input string, extra map[string]string, red harness.Redactor, cancelInput bool, wait time.Duration, sink harness.Sink) error {
	path, err := exec.LookPath(bin)
	if err != nil {
		return fmt.Errorf("claude executable: %w", err)
	}
	dir, err := os.MkdirTemp("", "fab-harness-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	env := map[string]string{"PATH": os.Getenv("PATH"), "HOME": os.Getenv("HOME")}
	for k, v := range extra {
		env[k] = v
	}
	a := &claudecode.Adapter{Launch: claudecode.ExecLauncher(path), StopGrace: 15 * time.Second, Redact: red}
	spec := harness.StartSpec{SessionID: fmt.Sprintf("ses_%d", time.Now().UnixNano()), Prompt: prompt, Dir: dir, Model: model, Env: env}
	s, err := a.Start(context.Background(), spec, sink)
	if err != nil {
		return err
	}
	// Whatever happens next, the session is stopped before this returns.
	ctx, cancel := context.WithTimeout(context.Background(), wait+90*time.Second)
	defer cancel()
	defer func() { _ = s.Stop(ctx) }()
	var failure error
	if cancelInput {
		cctx, ccancel := context.WithCancel(context.Background())
		ccancel()
		if err := s.Input(cctx, "(cancelled input, must be refused)"); !errors.Is(err, context.Canceled) {
			failure = fmt.Errorf("an Input with a cancelled context was not refused: %v", err)
		}
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	waited := make(chan struct{})
	go func() { _, _ = s.Wait(context.Background()); close(waited) }()
	rs, _ := s.(interface{ Results() int })
	// waitResults blocks until the session has produced n results, the wait
	// timer or a signal fires, or the child ends on its own.
	waitResults := func(n int) bool {
		tk := time.NewTicker(50 * time.Millisecond)
		defer tk.Stop()
		for {
			if rs != nil && rs.Results() >= n {
				return true
			}
			select {
			case <-timer.C:
				return false
			case <-sctx.Done():
				return false
			case <-waited:
				return rs != nil && rs.Results() >= n
			case <-tk.C:
			}
		}
	}
	want := 1 // the prompt
	if !waitResults(1) {
		failure = errors.Join(failure, errors.New("the first prompt got no result before the wait ended"))
	} else if input != "" {
		// The input goes in only after the first turn has finished, so a
		// result can only be the answer to it: a merged input cannot hide a
		// lost one.
		if err := s.Input(ctx, input); err != nil {
			failure = errors.Join(failure, fmt.Errorf("input: %w", err))
		} else {
			want = 2
			if !waitResults(2) {
				failure = errors.Join(failure, errors.New("the second input got no result: it was lost or unanswered"))
			}
		}
	}
	_ = want
	if sctx.Err() != nil {
		failure = errors.Join(failure, errors.New("interrupted by a signal: the scripted session did not complete"))
	}
	if err := s.Interrupt(ctx); err != nil {
		failure = errors.Join(failure, fmt.Errorf("interrupt: %w", err))
	}
	if err := s.Stop(ctx); err != nil {
		return errors.Join(failure, err)
	}
	f, err := s.Wait(ctx)
	fmt.Printf("session final: outcome=%s exit_code=%d reason=%q err=%v\n", f.Outcome, f.ExitCode, f.Reason, err)
	return errors.Join(failure, err)
}

// judge reads the trace back, checks it against the adapter contract and
// decides success. A deploy check can rely on the exit code: only a run that
// really finished its work returns nil.
//
// native: the final outcome must be done.
// claude-code: the scripted session is ended by Stop, so the final must be
// stopped with exit code 0; at least one assistant reply must exist; no turn
// may have ended in error; and no input may be reported undelivered.
func judge(adapter string, evs []harness.Event) error {
	counts := map[harness.Kind]int{}
	for _, e := range evs {
		counts[e.Kind]++
	}
	kinds := make([]string, 0, len(counts))
	for k, n := range counts {
		kinds = append(kinds, fmt.Sprintf("%s=%d", k, n))
	}
	sort.Strings(kinds)
	conf := harness.CheckStream(evs)
	fmt.Printf("trace: %d events (%s) conforms=%v\n", len(evs), strings.Join(kinds, " "), conf == nil)
	if conf != nil {
		return fmt.Errorf("trace does not conform: %w", conf)
	}
	var f harness.Final
	if err := json.Unmarshal(evs[len(evs)-1].Payload, &f); err != nil {
		return err
	}
	fmt.Printf("final: %s\n", f.Outcome)
	switch adapter {
	case "native":
		if f.Outcome != harness.OutcomeDone {
			return fmt.Errorf("native run ended %s", f.Outcome)
		}
	case "claude-code":
		if f.Outcome != harness.OutcomeStopped || f.ExitCode != 0 {
			return fmt.Errorf("claude-code session ended %s (exit code %d), want a clean stop", f.Outcome, f.ExitCode)
		}
		if strings.Contains(f.Reason, "undelivered_inputs=") {
			return fmt.Errorf("claude-code session left inputs undelivered")
		}
		replies := 0
		for _, e := range evs {
			if e.Kind != harness.KindMessage {
				continue
			}
			var m harness.Message
			if json.Unmarshal(e.Payload, &m) != nil {
				continue
			}
			switch {
			case m.Role == "assistant" && m.Text != "":
				replies++
			case m.Role == "system" && strings.HasPrefix(m.Text, "turn ended:"):
				return fmt.Errorf("claude-code turn ended in error: %s", m.Text)
			}
		}
		if replies == 0 {
			return errors.New("claude-code produced no assistant reply")
		}
		if strings.Contains(f.Reason, "may have been merged") {
			fmt.Println("note: some queued inputs were merged by claude or lost; check the reply count")
		}
	}
	return nil
}
