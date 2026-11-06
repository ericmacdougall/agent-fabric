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
	"runtime"
	"strings"
	"time"

	"fabric/internal/domain"
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
		return withStore(func(ctx context.Context, st *store.Store) error {
			if err := sched.RegisterJob(ctx, st, *id, domain.MissionID(*mission), domain.AttemptID(*attempt), *fence, argv, "fab-job-"+*id); err != nil {
				return err
			}
			watch := append([]string{self, "job", "watch", "--id", *id, "--rundir", *rundir, "--"}, argv...)
			var cmd *exec.Cmd
			if runtime.GOOS == "linux" {
				sr := []string{"--user", "--collect", "--quiet", "--unit", "fab-job-" + *id, "-p", "KillMode=control-group"}
				for _, e := range []string{"FABRIC_DSN=" + os.Getenv("FABRIC_DSN"), "FABRIC_HOME=" + os.Getenv("FABRIC_HOME"), "HOME=" + os.Getenv("HOME"), "PATH=" + os.Getenv("PATH")} {
					sr = append(sr, "-E", e)
				}
				sr = append(sr, "--")
				sr = append(sr, watch...)
				cmd = exec.Command("systemd-run", sr...)
				if out, err := cmd.CombinedOutput(); err != nil {
					st.Pool().Exec(ctx, "UPDATE jobs SET state = 'FAILED_TO_START', log_tail = $2 WHERE id = $1", *id, string(out)) //nolint:errcheck
					return fmt.Errorf("job unit failed to start: %v %s", err, out)
				}
			} else {
				cmd = exec.Command(watch[0], watch[1:]...)
				cmd.Env = os.Environ()
				cmd.SysProcAttr = detachAttr()
				if err := cmd.Start(); err != nil {
					st.Pool().Exec(ctx, "UPDATE jobs SET state = 'FAILED_TO_START', log_tail = $2 WHERE id = $1", *id, err.Error()) //nolint:errcheck
					return err
				}
				st.Pool().Exec(ctx, "UPDATE jobs SET pid = $2 WHERE id = $1", *id, cmd.Process.Pid) //nolint:errcheck
				go cmd.Wait()                                                                       //nolint:errcheck
			}
			fmt.Printf("job %s STARTED (wait token job:%s)\n", *id, *id)
			return nil
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
	ln, err := launcherForThisHost()
	if err != nil {
		return err
	}
	cfg := sched.Config{Host: domain.HostID(*hostID), Model: model, Effort: effort, Rung: *rung, TierTableSHA: sha, Profile: *profile,
		MaxTurns: 40, MaxBudgetUSD: 2, Wall: 45 * time.Minute, WorkRoot: *workRoot}
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
