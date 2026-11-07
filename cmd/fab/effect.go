package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"fabric/internal/domain"
	"fabric/internal/effects"
	"fabric/internal/store"
)

// attemptMain: `fab attempt start --mission M --id r_x --key K [--host H --role writer]`
func attemptMain(args []string) error {
	if len(args) == 0 || args[0] != "start" {
		return errors.New("attempt start --mission M --id R --path claude-code|anthropic-api [--lock-path P] --key K")
	}
	fs := flag.NewFlagSet("attempt start", flag.ContinueOnError)
	mission := fs.String("mission", "", "mission id")
	id := fs.String("id", "", "attempt id (r_...)")
	hostID := fs.String("host", "h_local", "host id")
	role := fs.String("role", "writer", "writer|reviewer")
	key := fs.String("key", "", "idempotency key")
	path := fs.String("path", "", "launcher path: claude-code or anthropic-api (required; must be the mission's provider)")
	lockPath := fs.String("lock-path", "", "anthropic-api only: the attempt's liveness lock file, an absolute path ending in /native-locks/<id>.lock")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	p, err := domain.ParseProvider(*path)
	if err != nil {
		return usageError{fmt.Errorf("attempt start: --path: %w", err)}
	}
	if p == domain.ProviderAnthropicAPI {
		// The CLI never takes or holds the flock, so such an attempt reads
		// as gone until a native process holds it (spec AC-2).
		if err := domain.ValidLockPath(domain.AttemptID(*id), *lockPath); err != nil {
			return usageError{fmt.Errorf("attempt start: --lock-path: %w", err)}
		}
	} else if *lockPath != "" {
		return usageError{errors.New("attempt start: --lock-path is for --path anthropic-api only")}
	}
	return withStore(func(ctx context.Context, st *store.Store) error {
		resp, err := execute(ctx, st, actor(), *key, domain.Command{Kind: domain.CmdStartAttempt, Mission: domain.MissionID(*mission),
			Attempt: domain.AttemptID(*id), Host: domain.HostID(*hostID), Role: *role, Path: p, LockPath: *lockPath})
		if err != nil {
			return err
		}
		fmt.Printf("attempt %s started: fence %d (replayed=%v)\n", *id, resp.Events[0].Fence, resp.Replayed)
		return nil
	})
}

func goodqaExecutor() (*effects.GoodQA, error) {
	endpoint := os.Getenv("FABRIC_GOODQA_ENDPOINT")
	if endpoint == "" {
		endpoint = "https://goodqa.app/v1/mcp"
	}
	keyFile := os.Getenv("FABRIC_GOODQA_KEY_FILE")
	if keyFile == "" {
		keyFile = filepath.Join(os.Getenv("HOME"), ".claude", "goodqa-hive", "member.key")
	}
	if st, err := os.Stat(keyFile); err != nil || st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("goodqa key file %s must exist with mode 0600", keyFile)
	}
	return &effects.GoodQA{Endpoint: endpoint, KeyFile: keyFile}, nil
}

// effectMain:
//
//	fab effect comment --mission M --attempt R --fence F --issue-id UUID --ticket goodqa#N --body TEXT --key K [--approve]
//	fab effect reconcile --id E   (app role: readback is not a runner step, 0018)
//	fab effect show --id E
//	fab effect unresolved [--age 10m]   (UNKNOWN, RECONCILING, and DISPATCHED of an ended attempt; then fab effect reconcile)
//	fab effect resolve --id E --outcome applied|not_applied --reason R --evidence REF   (app role; QUARANTINED only, audited)
//	fab effect abandon --id E --reason R --evidence REF   (app role; a never-dispatched intent whose attempt ended -> NOT_APPLIED, audited)
func effectMain(args []string) error {
	if len(args) == 0 {
		return errors.New("effect comment|reconcile|show|resolve|abandon|unresolved")
	}
	switch args[0] {
	case "unresolved":
		fs := flag.NewFlagSet("effect unresolved", flag.ContinueOnError)
		age := fs.Duration("age", 0, "only effects unchanged for at least this long")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return withStore(func(ctx context.Context, st *store.Store) error {
			ids, err := effects.New(st).Unresolved(ctx, *age, 200)
			if err != nil {
				return err
			}
			for _, id := range ids {
				fmt.Println(id)
			}
			return nil
		})
	case "abandon":
		fs := flag.NewFlagSet("effect abandon", flag.ContinueOnError)
		id := fs.String("id", "", "effect id")
		reason := fs.String("reason", "", "why the intent is abandoned")
		evidence := fs.String("evidence", "", "evidence reference (run record, WAL id)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return withStore(func(ctx context.Context, st *store.Store) error {
			e, err := effects.New(st).Abandon(ctx, *id, *reason, *evidence, actor())
			if err != nil {
				return err
			}
			fmt.Printf("effect %s %s (%s)\n", e.ID, e.State, e.LastError)
			return nil
		})
	case "resolve":
		fs := flag.NewFlagSet("effect resolve", flag.ContinueOnError)
		id := fs.String("id", "", "effect id")
		outcome := fs.String("outcome", "", "applied or not_applied: what the operator established")
		reason := fs.String("reason", "", "how the outcome was established")
		evidence := fs.String("evidence", "", "evidence reference (readback log, WAL id, remote record)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		to := map[string]effects.State{"applied": effects.ResolvedApplied, "not_applied": effects.ResolvedNotApplied}[*outcome]
		if to == "" {
			return errors.New("--outcome must be applied or not_applied")
		}
		return withStore(func(ctx context.Context, st *store.Store) error {
			e, err := effects.New(st).ResolveQuarantined(ctx, *id, to, *reason, *evidence, actor())
			if err != nil {
				return err
			}
			fmt.Printf("effect %s %s (%s)\n", e.ID, e.State, e.LastError)
			return nil
		})
	case "comment":
		fs := flag.NewFlagSet("effect comment", flag.ContinueOnError)
		mission := fs.String("mission", "", "mission id")
		attempt := fs.String("attempt", "", "attempt id")
		fence := fs.Int64("fence", 0, "attempt fence")
		issueID := fs.String("issue-id", "", "GoodQA issue uuid")
		ticket := fs.String("ticket", "", "ticket ref, e.g. goodqa#14913")
		body := fs.String("body", "", "comment body (the trailer is appended by the executor)")
		key := fs.String("key", "", "idempotency key (also the remote operationId)")
		approve := fs.Bool("approve", false, "require and record an approval bound to the request hash before dispatch")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		g, err := goodqaExecutor()
		if err != nil {
			return err
		}
		req, _ := json.Marshal(map[string]string{"issue_id": *issueID, "ticket": *ticket, "body": *body})
		return withStore(func(ctx context.Context, st *store.Store) error {
			b := effects.New(st)
			e, err := b.Submit(ctx, g, effects.IntentSpec{Mission: domain.MissionID(*mission), Attempt: domain.AttemptID(*attempt), Fence: *fence,
				Kind: "ticket_comment", Subject: "ticket:" + *ticket, IdemKey: *key, Request: req, NeedsApproval: *approve})
			if err != nil {
				return err
			}
			fmt.Printf("effect %s %s (replayed=%v) request_sha=%s\n", e.ID, e.State, e.Replayed, e.RequestSHA[:12])
			if e.State == effects.ApprovalRequired {
				if err := b.Approve(ctx, e.ID, e.RequestSHA, actor()); err != nil {
					return err
				}
				fmt.Printf("effect %s approved for request_sha %s\n", e.ID, e.RequestSHA[:12])
			}
			if e.State == effects.Confirmed || e.State == effects.Unknown || e.State == effects.Dispatched || e.State == effects.Reconciling || e.State == effects.Quarantined {
				fmt.Printf("effect %s already %s; not dispatching again\n", e.ID, e.State)
				return nil
			}
			if e.State == effects.NotApplied || e.State == effects.FailedDefinite {
				if e, err = b.Retry(ctx, e.ID); err != nil {
					return err
				}
				fmt.Printf("effect %s re-armed as %s under the same key\n", e.ID, e.State)
			}
			e, err = b.Dispatch(ctx, g, e.ID)
			if err != nil {
				return err
			}
			fmt.Printf("effect %s -> %s dispatches=%d detail=%s receipt=%s\n", e.ID, e.State, e.Dispatches, e.LastError, string(e.Receipt))
			return nil
		})
	case "reconcile", "show":
		fs := flag.NewFlagSet("effect "+args[0], flag.ContinueOnError)
		id := fs.String("id", "", "effect id")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return withStore(func(ctx context.Context, st *store.Store) error {
			b := effects.New(st)
			var e effects.Effect
			var err error
			if args[0] == "show" {
				e, err = b.Load(ctx, *id)
			} else {
				g, gerr := goodqaExecutor()
				if gerr != nil {
					return gerr
				}
				e, err = b.Reconcile(ctx, g, *id)
			}
			if err != nil {
				return err
			}
			fmt.Printf("effect %s %s dispatches=%d key=%s detail=%s receipt=%s\n", e.ID, e.State, e.Dispatches, e.IdemKey, e.LastError, string(e.Receipt))
			return nil
		})
	}
	return errors.New("effect comment|reconcile|show")
}
