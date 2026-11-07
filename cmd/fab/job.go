package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"fabric/internal/domain"
	"fabric/internal/host"
	"fabric/internal/sched"
	"fabric/internal/store"
)

// jobMain:
//
//	fab job run --mission M --attempt R --fence F --rundir D [--id J] -- argv...
//	fab job watch --id J --rundir D -- argv...      (the host-side watcher; internal)
//	fab job show --id J
//
// A job is host-owned: it runs in its own transient unit on Linux (so it
// outlives the worker run and is stopped with the mission), and its exit is
// recorded as a row plus a wake hint. STARTED is only reported after the
// unit or process is proven to exist (bg_job's contract, DESIGN.md 3.4).
func jobMain(args []string) error {
	if len(args) == 0 {
		return errors.New("job run|watch|show")
	}
	switch args[0] {
	case "run":
		fs := flag.NewFlagSet("job run", flag.ContinueOnError)
		mission := fs.String("mission", os.Getenv("FAB_MISSION"), "mission id")
		attempt := fs.String("attempt", os.Getenv("FAB_ATTEMPT"), "attempt id")
		fence := fs.Int64("fence", envInt(os.Getenv("FAB_FENCE")), "attempt fence")
		rundir := fs.String("rundir", os.Getenv("FAB_RUN_DIR"), "run directory for logs")
		id := fs.String("id", "", "job id (default: minted)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		argv := fs.Args()
		if len(argv) == 0 || *rundir == "" {
			return errors.New("job run needs --rundir and -- argv")
		}
		if *id == "" {
			*id = fmt.Sprintf("j_%s_%d", strings.TrimPrefix(*attempt, "r_"), time.Now().UnixNano()%1_000_000)
		}
		self, err := os.Executable()
		if err != nil {
			return err
		}
		_ = self
		// The worker only requests the job; the daemon, outside the sandbox,
		// starts it in its own unit (review C1). STARTED is printed only once
		// the daemon has proven the unit exists (bg_job's contract).
		return withStore(func(ctx context.Context, st *store.Store) error {
			if err := sched.RequestJob(ctx, st, *id, domain.MissionID(*mission), domain.AttemptID(*attempt), *fence, argv, *rundir); err != nil {
				return err
			}
			sched.Notify(ctx, st)
			deadline := time.Now().Add(20 * time.Second)
			for time.Now().Before(deadline) {
				j, err := sched.LoadJob(ctx, st, *id)
				if err != nil {
					return err
				}
				switch j.State {
				case "STARTED", "EXITED":
					fmt.Printf("job %s STARTED (wait token job:%s)\n", *id, *id)
					return nil
				case "FAILED_TO_START":
					return fmt.Errorf("job %s failed to start: %s", *id, j.LogTail)
				}
				time.Sleep(200 * time.Millisecond)
			}
			return fmt.Errorf("job %s: the daemon did not start it within 20s (is fab daemon running?)", *id)
		})
	case "watch":
		fs := flag.NewFlagSet("job watch", flag.ContinueOnError)
		id := fs.String("id", "", "job id")
		rundir := fs.String("rundir", "", "run directory")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		argv := fs.Args()
		if *id == "" || len(argv) == 0 {
			return errors.New("job watch --id J --rundir D -- argv")
		}
		os.MkdirAll(filepath.Join(*rundir, "jobs"), 0o755) //nolint:errcheck
		logf, err := os.Create(filepath.Join(*rundir, "jobs", *id+".log"))
		if err != nil {
			return err
		}
		var tail bytes.Buffer
		w := io.MultiWriter(logf, &tail)
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Stdout, cmd.Stderr = w, w
		cmd.Env = os.Environ()
		err = cmd.Run()
		logf.Close()
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else if err != nil {
			code = 127
			tail.WriteString(err.Error())
		}
		return withStore(func(ctx context.Context, st *store.Store) error {
			return sched.FinishJob(ctx, st, *id, code, tail.String())
		})
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

func hostname() string {
	h, _ := os.Hostname()
	if i := strings.IndexByte(h, '.'); i > 0 {
		h = h[:i]
	}
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, strings.ToLower(h))
}

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
