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
// scripted from flags. Other adapters land in their own PRs.
func harnessMain(args []string) error {
	if len(args) == 0 || args[0] != "run" {
		return errors.New("usage: fab harness run --adapter native|claude-code --events FILE ...")
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

func harnessRunMain(args []string) error {
	fs := flag.NewFlagSet("harness run", flag.ContinueOnError)
	adapter := fs.String("adapter", "", "harness adapter (native, claude-code)")
	grantFile := fs.String("grant", "", "native: grant file")
	events := fs.String("events", "", "new file to write the event trace to (JSONL, mode 0600)")
	fake := fs.Bool("unconfined-fake-host", false, "tests only: run tools without containment (needs FABRIC_ALLOW_FAKE_HOST=1)")
	slow := fs.Duration("sink-delay", 0, "delay every event write by this long (exercises a slow sink)")
	claudeBin := fs.String("claude-bin", "claude", "claude-code: the claude executable")
	prompt := fs.String("prompt", "", "claude-code: first prompt")
	input := fs.String("input", "", "claude-code: a second message sent after the first reply")
	cancelInput := fs.Bool("cancel-input", false, "claude-code: also send an Input whose context is cancelled mid-call")
	passEnv := fs.String("pass-env", "", "claude-code: comma-separated names of extra environment variables to pass (names only; PATH and HOME are always passed)")
	model := fs.String("model", "", "claude-code: model name")
	wait := fs.Duration("wait", 120*time.Second, "claude-code: how long to wait for replies before Stop")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *events == "" {
		return errors.New("--events is required")
	}
	sink, err := harness.NewJSONLSink(*events)
	if err != nil {
		return err
	}
	var out harness.Sink = sink
	if *slow > 0 {
		out = slowSink{next: sink, delay: *slow}
	}
	var runErr error
	switch *adapter {
	case "native":
		runErr = runNative(*grantFile, *fake, out)
	case "claude-code":
		runErr = runClaudeCode(*claudeBin, *model, *prompt, *input, *passEnv, *cancelInput, *wait, out)
	default:
		runErr = fmt.Errorf("--adapter %q is not wired into fab harness run (native, claude-code)", *adapter)
	}
	if cerr := sink.Close(); cerr != nil && runErr == nil {
		runErr = cerr
	}
	return errors.Join(runErr, report(*events))
}

func runNative(grantFile string, fake bool, sink harness.Sink) error {
	g, err := native.LoadGrant(grantFile)
	if err != nil {
		return err
	}
	opts, err := nativeOptions(g, fake)
	if err != nil {
		return err
	}
	opts.Sink = sink
	// SIGINT or SIGTERM is Stop: the run ends and settles as stopped.
	sctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
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
// Input cancelled mid-call (the session must survive it), a real second
// Input, an Interrupt, and a Stop. The environment given to claude is PATH,
// HOME and the variables named by --pass-env.
func runClaudeCode(bin, model, prompt, input, passEnv string, cancelInput bool, wait time.Duration, sink harness.Sink) error {
	if prompt == "" {
		return errors.New("--prompt is required for claude-code")
	}
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
	for _, k := range strings.Split(passEnv, ",") {
		if k = strings.TrimSpace(k); k != "" {
			if v, ok := os.LookupEnv(k); ok {
				env[k] = v
			}
		}
	}
	a := &claudecode.Adapter{Launch: claudecode.ExecLauncher(path), StopGrace: 15 * time.Second}
	spec := harness.StartSpec{SessionID: fmt.Sprintf("ses_%d", time.Now().UnixNano()), Prompt: prompt, Dir: dir, Model: model,
		Env: env}
	s, err := a.Start(context.Background(), spec, sink)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait+60*time.Second)
	defer cancel()
	if cancelInput {
		cctx, ccancel := context.WithCancel(context.Background())
		go ccancel()
		err := s.Input(cctx, "(cancelled input, ignore)")
		fmt.Printf("cancelled input: %v\n", err)
	}
	if input != "" {
		if err := s.Input(ctx, input); err != nil {
			fmt.Printf("input: %v\n", err)
		}
	}
	time.Sleep(wait)
	fmt.Printf("interrupt: %v\n", s.Interrupt(ctx))
	if err := s.Stop(ctx); err != nil {
		return err
	}
	f, err := s.Wait(ctx)
	fmt.Printf("session final: %+v err=%v\n", f, err)
	return err
}

// report reads the trace back and checks it against the adapter contract.
func report(path string) error {
	evs, err := harness.ReadJSONL(path)
	if err != nil {
		return err
	}
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
	if len(evs) > 0 {
		last := evs[len(evs)-1]
		var f harness.Final
		if last.Kind == harness.KindFinal && json.Unmarshal(last.Payload, &f) == nil {
			fmt.Printf("final: %s\n", f.Outcome)
		}
	}
	if conf != nil {
		return fmt.Errorf("trace does not conform: %w", conf)
	}
	return nil
}
