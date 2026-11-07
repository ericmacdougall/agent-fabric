package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"fabric/internal/agent"
	"fabric/internal/native"
	"fabric/internal/policy"
	"fabric/internal/provider/anthropic"
)

func providerMain(args []string) error {
	if len(args) == 0 || args[0] != "capture" {
		return errors.New("provider capture --grant F --out DIR [--scenarios a,b]")
	}
	fs := flag.NewFlagSet("provider capture", flag.ContinueOnError)
	grantFile := fs.String("grant", "", "grant file (a live-test grant: caps.run_usd_micros <= 1 USD)")
	out := fs.String("out", "", "directory for fixtures")
	scen := fs.String("scenarios", "text,tool,thinking,badauth,badrequest", "scenarios to record")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	g, err := native.LoadGrant(*grantFile)
	if err != nil {
		return err
	}
	if !g.LiveTest {
		return errors.New("capture spends real money: the grant must set live_test (capped at 1 USD)")
	}
	prices, err := agent.ParsePrices(g.Prices)
	if err != nil {
		return err
	}
	keySrc, err := anthropic.KeyFromCommand(g.KeyCommand, 5*time.Minute)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	key, err := keySrc(ctx)
	if err != nil {
		return err
	}
	red := policy.Redactor{Literals: []string{key}}

	var held int64 // worst-case total across all requests; the cap is enforced before each send
	for _, name := range strings.Split(*scen, ",") {
		name = strings.TrimSpace(name)
		body, ks, err := captureRequest(name, g, keySrc)
		if err != nil {
			return err
		}
		var cap anthropic.Capture
		cl, err := anthropic.New(anthropic.Config{BaseURL: g.BaseURL, Version: g.AnthropicVersion, Key: ks,
			Retry:   anthropic.RetryPolicy{MaxAttempts: 1, BaseBackoff: time.Second, MaxBackoff: time.Second, MaxRetryAfter: time.Second},
			Capture: func(c anthropic.Capture) { cap = c }})
		if err != nil {
			return err
		}
		_, _, sendErr := cl.Send(ctx, body, anthropic.Hooks{BeforeAttempt: func(int) error {
			held += prices.WorstCase(len(body), g.MaxTokens)
			if held > g.Caps.Run {
				return fmt.Errorf("capture: worst-case total %d exceeds the %d micro-USD cap; stopping before %s", held, g.Caps.Run, name)
			}
			return nil
		}})
		if cap.Status == 0 {
			return fmt.Errorf("scenario %s: no response captured: %w", name, sendErr)
		}
		fx := map[string]any{
			"scenario": name, "provenance": "real", "captured_at": time.Now().UTC().Format(time.RFC3339),
			"anthropic_version": g.AnthropicVersion, "status": cap.Status, "request_id": cap.RequestID,
			"headers": cap.Header, "request": json.RawMessage(cap.RequestBody), "response": string(cap.ResponseBody),
		}
		raw, _ := json.MarshalIndent(fx, "", "  ")
		// The redaction scan runs before anything touches the disk: a fixture
		// that matches a secret shape or the key itself is not written.
		if kinds := policy.Find(string(raw)); len(kinds) > 0 || red.Redact(string(raw)) != string(raw) {
			return fmt.Errorf("scenario %s: fixture matched secret shapes %v; not written", name, kinds)
		}
		path := filepath.Join(*out, name+".json")
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			return err
		}
		fmt.Printf("%s: status %d request-id %s -> %s (worst-case held so far %d micro-USD)\n", name, cap.Status, cap.RequestID, path, held)
	}
	return nil
}

func captureRequest(name string, g native.Grant, real anthropic.KeySource) ([]byte, anthropic.KeySource, error) {
	user := func(s string) []anthropic.Msg {
		b, _ := json.Marshal(map[string]string{"type": "text", "text": s})
		return []anthropic.Msg{{Role: "user", Content: []json.RawMessage{b}}}
	}
	spec := anthropic.RequestSpec{Model: g.Model, MaxTokens: g.MaxTokens}
	ks := real
	switch name {
	case "text":
		spec.Messages = user("Reply with the single word: ok")
	case "tool":
		spec.Tools = []json.RawMessage{json.RawMessage(`{"name":"read","description":"Read a file in the worktree.","input_schema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}`)}
		spec.Messages = user("Call the read tool with path notes.txt. Do not write anything else.")
	case "thinking":
		spec.Extra = g.Extra
		spec.Messages = user("What is 17 times 23? Answer with only the number.")
	case "badauth":
		spec.Messages = user("hi")
		ks = func(context.Context) (string, error) { return "invalid-key-for-fixture-000000000000", nil }
	case "badrequest":
		b := fmt.Sprintf(`{"model":%q,"max_tokens":1,"stream":true,"messages":[]}`, g.Model)
		return []byte(b), ks, nil
	default:
		return nil, nil, fmt.Errorf("unknown scenario %q", name)
	}
	b, err := anthropic.BuildRequest(spec)
	return b, ks, err
}
