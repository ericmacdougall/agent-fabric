package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"fabric/internal/domain"
	"fabric/internal/store"
)

// stringList is a repeatable flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

const askUsage = "ask new|answer|withdraw|show|repair"

// askMain is the inbox write path (DESIGN.md 3.11, AC-10). Every ask and every
// answer carries a source in the provenance grammar (R12); an unsourced one is
// refused with exit 3 and nothing is stored.
func askMain(args []string) error {
	if len(args) == 0 {
		return errors.New(askUsage)
	}
	switch args[0] {
	case "new":
		fs := flag.NewFlagSet("ask new", flag.ContinueOnError)
		id := fs.String("id", "", "ask id (1..48 of [A-Za-z0-9._-])")
		mission := fs.String("mission", "", "mission the ask blocks (optional); it resumes on the answer")
		kind := fs.String("kind", "decision", "decision|permission|credential-link")
		question := fs.String("question", "", "the question")
		recommended := fs.String("recommended", "", "the recommended answer")
		source := fs.String("source", "", "provenance (R12 grammar; required)")
		var options stringList
		fs.Var(&options, "option", "an option (repeatable)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *question == "" {
			return errors.New("--question is required")
		}
		return withStore(func(ctx context.Context, st *store.Store) error {
			err := st.CreateAsk(ctx, store.Ask{ID: *id, Mission: domain.MissionID(*mission), Kind: *kind, Question: *question,
				Options: options, Recommended: *recommended, Source: *source})
			if err != nil {
				return refusal(err)
			}
			label := ""
			if strings.HasPrefix(*source, "inferred:") {
				label = " (assumption: inferred source)"
			}
			fmt.Printf("ask %s open%s; wait token %s\n", *id, label, store.AskToken(*id))
			return nil
		})
	case "answer":
		fs := flag.NewFlagSet("ask answer", flag.ContinueOnError)
		answer := fs.String("answer", "", "the answer")
		source := fs.String("source", "", "provenance of the answer (R12 grammar; required)")
		id, err := parseWithID(fs, args[1:], "ask answer <id> --answer A --source S")
		if err != nil {
			return err
		}
		return withStore(func(ctx context.Context, st *store.Store) error {
			res, err := st.AnswerAsk(ctx, id, *answer, actor(), *source)
			var pending *store.WakePendingError
			if errors.As(err, &pending) {
				fmt.Printf("ask %s answered (replayed=%v); wake pending\n", id, res.Replayed)
				return err // exit 1: the answer is stored, the wake is not (not a refusal)
			}
			if err != nil {
				return refusal(err)
			}
			wake := "no mission to wake"
			if res.Wake != nil {
				wake = fmt.Sprintf("wake %s on %s: %d events, replayed=%v", store.AskToken(id), res.Ask.Mission, len(res.Wake.Events), res.Wake.Replayed)
			}
			fmt.Printf("ask %s answered (replayed=%v); %s\n", id, res.Replayed, wake)
			return nil
		})
	case "withdraw":
		fs := flag.NewFlagSet("ask withdraw", flag.ContinueOnError)
		reason := fs.String("reason", "", "why the ask is no longer needed (required)")
		id, err := parseWithID(fs, args[1:], "ask withdraw <id> --reason R")
		if err != nil {
			return err
		}
		return withStore(func(ctx context.Context, st *store.Store) error {
			if err := st.WithdrawAsk(ctx, id, *reason); err != nil {
				return refusal(err)
			}
			fmt.Printf("ask %s withdrawn\n", id)
			return nil
		})
	case "show":
		fs := flag.NewFlagSet("ask show", flag.ContinueOnError)
		id, err := parseWithID(fs, args[1:], "ask show <id>")
		if err != nil {
			return err
		}
		return withStore(func(ctx context.Context, st *store.Store) error {
			a, err := st.GetAsk(ctx, id)
			if err != nil {
				return err
			}
			return printJSON(a)
		})
	case "repair":
		fs := flag.NewFlagSet("ask repair", flag.ContinueOnError)
		limit := fs.Int("limit", 100, "page size")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return withStore(func(ctx context.Context, st *store.Store) error {
			n, err := st.RepairAskWakes(ctx, *limit)
			fmt.Printf("ask repair: %d missing wakes fired\n", n)
			return err
		})
	}
	return errors.New(askUsage)
}

// missionShow prints one mission's snapshot and its event log.
func missionShow(args []string) error {
	fs := flag.NewFlagSet("mission show", flag.ContinueOnError)
	events := fs.Bool("events", true, "include the event log")
	id, err := parseWithID(fs, args, "mission show <id>")
	if err != nil {
		return err
	}
	return withStore(func(ctx context.Context, st *store.Store) error {
		m, err := st.Mission(ctx, domain.MissionID(id))
		if err != nil {
			return err
		}
		prov, err := st.MissionProvider(ctx, domain.MissionID(id))
		if err != nil {
			return err
		}
		out := map[string]any{"mission": m, "provider": prov}
		if *events {
			evs, err := st.Events(ctx, domain.MissionID(id))
			if err != nil {
				return err
			}
			out["events"] = evs
		}
		return printJSON(out)
	})
}

// parseWithID parses flags around exactly one positional id, which may come
// before or after the flags (the flag package alone stops at the first
// positional and would silently ignore every flag after the id).
func parseWithID(fs *flag.FlagSet, args []string, usage string) (string, error) {
	var id string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		id, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if id == "" && fs.NArg() == 1 {
		id = fs.Arg(0)
	} else if fs.NArg() != 0 || id == "" {
		return "", errors.New("usage: fab " + usage)
	}
	return id, nil
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// refusal turns a domain refusal into *store.Refusal so main exits 3 (a
// refusal, nothing changed) rather than 1 (an error of unknown effect).
func refusal(err error) error {
	for _, e := range []error{domain.ErrInvalidTransition, domain.ErrInvalidID, domain.ErrIdempotencyConflict, domain.ErrNotFound} {
		if errors.Is(err, e) {
			return &store.Refusal{Response: store.Response{Status: "refused", Reason: err.Error()}, Err: err}
		}
	}
	return err
}
