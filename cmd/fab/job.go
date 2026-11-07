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

	"fabric/internal/domain"
	"fabric/internal/host"
	"fabric/internal/hostid"
	"fabric/internal/native"
	"fabric/internal/sched"
	"fabric/internal/store"
)

// jobMain:
//
//	fab job show --id J
//
// `fab job run` and `fab job watch` are refused (exit 3): host jobs are
// disabled (goodqa#15082 F0), because the run chose the argv and the
// daemon launched it outside the sandbox. `show` reads legacy rows.
func jobMain(args []string) error {
	if len(args) == 0 {
		return errors.New("job run|watch|show")
	}
	switch args[0] {
	case "run", "watch":
		// goodqa#15082 F0: host jobs are disabled. Refuse before parsing
		// flags, touching the filesystem, the database or running anything.
		return refusal(sched.ErrHostJobsDisabled)
	case "show":
		fs := flag.NewFlagSet("job show", flag.ContinueOnError)
		id := fs.String("id", "", "job id")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return withStore(func(ctx context.Context, st *store.Store) error {
			j, err := sched.LoadJob(ctx, st, *id)
			if err != nil {
				return err
			}
			b, _ := json.Marshal(j)
			fmt.Println(string(b))
			return nil
		})
	}
	return errors.New("job run|watch|show")
}

func selfPath() string {
	p, err := os.Executable()
	if err != nil {
		return "fab"
	}
	return p
}

func envInt(s string) int64 {
	var n int64
	fmt.Sscanf(s, "%d", &n) //nolint:errcheck
	return n
}

// daemonMain: `fab daemon --work-root DIR --provider BIN --tier-table FILE --rung R0 [--profile P1] [--once]`
// The model and effort come from the tier table file, never from a flag or a literal (R23).
func daemonMain(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	workRoot := fs.String("work-root", "", "directory for worktrees and run dirs")
	provider := fs.String("provider", "", "provider binary (claude, or a recorded fake for shadow runs)")
	tierTable := fs.String("tier-table", "", "model_tiers.json")
	rung := fs.String("rung", "", "writer rung, e.g. R0")
	profile := fs.String("profile", "P1", "execution profile")
	once := fs.Bool("once", false, "run ticks until idle, then exit (tests)")
	hostID := fs.String("host", "h_"+hostname(), "host id")
	trustProvider := fs.Bool("trust-provider", false, "the provider is a trusted recorded fake; skip the containment gate")
	workerUser := fs.String("worker-user", os.Getenv("FABRIC_WORKER_USER"), "dedicated worker account (Linux); empty means same-UID shadow mode")
	var holds holdFlags
	fs.Var(&holds, "hold", "turn on the dispatch gate: hold resource:unit:amount per attempt (repeatable)")
	runnerDSNEnv := fs.String("runner-dsn-env", "FABRIC_RUNNER_DSN", "environment variable holding the runner role DSN (required with --hold)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *workRoot == "" || *provider == "" || *tierTable == "" || *rung == "" {
		return errors.New("--work-root, --provider, --tier-table and --rung are required")
	}
	model, effort, sha, err := resolveRung(*tierTable, *rung)
	if err != nil {
		return err
	}
	os.Setenv("FABRIC_WORKER_USER", *workerUser)
	ln, err := launcherForThisHost()
	if err != nil {
		return err
	}
	// An untrusted provider (anything but the recorded fake) runs only on a
	// host whose negative controls demonstrated containment (review C1).
	if !*trustProvider {
		p, err := profileForThisHost()
		if err != nil {
			return err
		}
		results, err := p.NegativeControls(filepath.Join(*workRoot, "controls"))
		if err != nil {
			return err
		}
		if containment, failed := host.Capabilities(results); !containment {
			return fmt.Errorf("refusing to launch an untrusted provider: this host did not demonstrate containment (failing controls: %s); use --trust-provider only for the recorded fake", strings.Join(failed, ", "))
		}
	}
	cfg := sched.Config{Host: domain.HostID(*hostID), Model: model, Effort: effort, Rung: *rung, TierTableSHA: sha, Profile: *profile,
		MaxTurns: 40, MaxBudgetUSD: 2, Wall: 45 * time.Minute, WorkRoot: *workRoot, FabBin: selfPath()}
	cfg.Launch.Binary = *provider
	cfg.Holds = holds
	cfg.RunnerDSN = os.Getenv(*runnerDSNEnv)
	cfg.Launch.SchemaPath = filepath.Join(*workRoot, "checkpoint.schema.json")
	cfg.Launch.SettingSources = ""
	cfg.Launch.StrictMCP, cfg.Launch.DisableSlash = true, true
	cfg.Launch.DisallowedTools = []string{"Agent", "Workflow", "AskUserQuestion", "WebFetch", "WebSearch"}
	if err := os.WriteFile(cfg.Launch.SchemaPath, []byte(checkpointSchema), 0o644); err != nil {
		return err
	}
	return withStore(func(ctx context.Context, st *store.Store) error {
		s := sched.New(st, cfg, ln)
		if p, ok := runnerProber(*workerUser); ok {
			s.SetProber(p) // G1: reconcile UNKNOWN attempts whose runner is gone
		}
		// goodqa#15087: native attempts read back through their stored lock
		// path, and fabd holds the host reconcile lock for its whole life.
		s.SetLockProber(native.ProbeLock)
		s.RequireReconcileLock()
		defer s.Close()
		if *once {
			for i := 0; i < 50; i++ {
				worked, err := s.Tick(ctx)
				if err != nil {
					return err
				}
				if !worked {
					break
				}
			}
			fmt.Printf("daemon once: launched=%d waits_resumed=%d jobs_woken=%d expired=%d\n", s.Launched, s.WaitsResumed, s.JobsWoken, s.Expired)
			return nil
		}
		return s.Run(ctx)
	})
}

const checkpointSchema = `{"type":"object","required":["end"],"properties":{"next":{"type":"string","maxLength":200},
"end":{"type":"object","required":["kind"],"properties":{"kind":{"enum":["checkpoint","wait","ready_for_review","blocked"]},"event":{"type":"string"},"reason":{"type":"string","maxLength":200}}}}}`

// hostname is the shared default host name (goodqa#15087: fabd and the
// native runner name a host the same way).
func hostname() string { return hostid.Name() }

// resolveRung reads the tier table file and returns (model, effort, sha256 of the file).
func resolveRung(path, rung string) (string, string, string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", "", "", err
	}
	var t struct {
		Rungs map[string]struct {
			Effort string            `json:"effort"`
			Models map[string]string `json:"models"`
		} `json:"rungs"`
	}
	if err := json.Unmarshal(b, &t); err != nil {
		return "", "", "", err
	}
	r, ok := t.Rungs[rung]
	if !ok || r.Models["claude"] == "" || r.Effort == "" {
		return "", "", "", fmt.Errorf("tier table has no claude model for rung %s", rung)
	}
	return r.Models["claude"], r.Effort, domainSHA(b), nil
}

func domainSHA(b []byte) string {
	base, _ := domain.EffectBase("tier-table", json.RawMessage(`"`+strings.ReplaceAll(string(b), `"`, `\"`)+`"`))
	if len(base) > 16 {
		return base[:16]
	}
	return base
}
