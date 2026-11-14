package main

import (
	"github.com/jackc/pgx/v5"

	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"fabric/internal/dispatch"
	"fabric/internal/domain"
	"fabric/internal/host"
	"fabric/internal/hostid"
	"fabric/internal/native"
	"fabric/internal/provider/claude"
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
		return errors.New("job run|watch|show|dispatch-candidates")
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
	case "dispatch-candidates":
		// goodqa#15373 live-step gate: the missions dispatch would start now,
		// from the scheduler's own query, inside a READ ONLY transaction.
		fs := flag.NewFlagSet("job dispatch-candidates", flag.ContinueOnError)
		only := fs.String("only-mission", "", "restrict to this mission id, as fab daemon --only-mission does")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return withStore(func(ctx context.Context, st *store.Store) error {
			tx, err := st.Pool().BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
			if err != nil {
				return err
			}
			defer tx.Rollback(ctx) //nolint:errcheck
			ids, err := sched.DispatchCandidates(ctx, tx, nil, domain.MissionID(*only))
			if err != nil {
				return err
			}
			for _, id := range ids {
				fmt.Println(id)
			}
			return nil
		})
	}
	return errors.New("job run|watch|show|dispatch-candidates")
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
	noDispatch := fs.Bool("no-dispatch", false, "consume the outbox and run the other sweeps but never start an attempt (the live fabd install, goodqa#15587)")
	workerUser := fs.String("worker-user", os.Getenv("FABRIC_WORKER_USER"), "dedicated worker account (Linux); empty means same-UID shadow mode")
	var holds holdFlags
	fs.Var(&holds, "hold", "turn on the dispatch gate: hold resource:unit:amount per attempt (repeatable)")
	runnerDSNEnv := fs.String("runner-dsn-env", "FABRIC_RUNNER_DSN", "environment variable holding the runner role DSN (required for every launch)")
	dispatchVia := fs.String("dispatch-via", "", "launch path: argv (default) or registry (prompt on stdin; the pinned stdin fake only, goodqa#15373)")
	launchLimit := fs.Int("launch-limit", 0, "stop physical starts after this many in this process (0: no limit)")
	onlyMission := fs.String("only-mission", "", "dispatch only this mission id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *workRoot == "" || *provider == "" || *tierTable == "" || *rung == "" {
		return errors.New("--work-root, --provider, --tier-table and --rung are required")
	}
	if *dispatchVia == sched.DispatchRegistry && !*trustProvider {
		return errors.New("refusing --dispatch-via=registry without --trust-provider: it runs only the pinned stdin fake (goodqa#15373 NEG-11)")
	}
	model, effort, sha, err := resolveRung(*tierTable, *rung)
	if err != nil {
		return err
	}
	// goodqa#15161 review R1: the provider is resolved once, here, before
	// anything else. An unresolvable provider refuses fabd's start with a
	// named cause on every host and trust level: the containment control
	// (provider_starts_under_profile) would otherwise have no file to run
	// and be skipped, and every later launch would be refused one by one.
	// newDaemonProvider also builds the scheduler's launch from the
	// operator's name, so no call site can hand it the start-time file
	// (review S1 on 2abd23a).
	schemaPath := filepath.Join(*workRoot, "checkpoint.schema.json")
	hooksDir := os.Getenv("FABRIC_HOOKS_DIR")
	dp, err := newDaemonProvider(*provider, schemaPath, hooksDir)
	if err != nil {
		return err
	}
	os.Setenv("FABRIC_WORKER_USER", *workerUser)
	os.Setenv("FABRIC_RUNNER_DSN_ENV", *runnerDSNEnv) // its port joins the worker's TCP allow-list (goodqa#15161)
	// goodqa#15976 AC-2, AC-3: a real provider (any file that does not hash
	// to a pinned fake) needs the Linux worker manager, a worker account and
	// a supporting systemd, and --trust-provider is honoured only for a
	// pinned fake. Checked before anything is written and before any control
	// starts a unit; a fake-only start re-checks the file at every launch.
	gateProfile, err := profileForThisHost()
	if err != nil {
		return err
	}
	realProvider, err := gateDaemonProvider(&dp, *noDispatch, *trustProvider, gateProfile)
	if err != nil {
		return err
	}
	// The schema is written before the profile and the controls, so the
	// launch_inputs_readable control sees the file a real launch reads.
	// The controls used to create the work root; the schema is now written
	// first, so create it here (review 716fc04). 0755, not 0775: the sealed
	// provider copies live under it and every ancestor of them must be
	// writable only by fabd (goodqa#15976 delta review F1).
	if err := os.MkdirAll(*workRoot, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(schemaPath, []byte(checkpointSchema), 0o644); err != nil {
		return err
	}
	// goodqa#15976 F1: the directory of sealed fake copies exists before
	// any profile is built, so both profiles can grant it read-only.
	if err := ownedSealDir(dp.SealRoot); err != nil {
		return err
	}
	// Launches exec the resolved path, so the read root is resolved too. A
	// fake-only daemon refuses at start when another uid could replace a
	// directory above the seal (delta review F1); each launch checks again.
	if real, err := filepath.EvalSymlinks(dp.SealRoot); err == nil {
		dp.SealRoot = real
	}
	if !realProvider {
		if _, err := trustedAncestry(dp.SealRoot); err != nil {
			return fmt.Errorf("refusing to start (goodqa#15976): %w", err)
		}
	}
	sealedProviderRO = []string{dp.SealRoot}
	// The read-allow-back and the control use the same start-time
	// resolution; each launch resolves the name again (productionLaunch),
	// so an update that repoints the symlink is followed (goodqa#15161
	// reviews C and N1).
	darwinLaunchRO = append(launchInputs(dp.Path, hooksDir, schemaPath), dp.SealRoot)
	darwinProvider = dp.Path
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
	cfg.Holds = holds
	cfg.NoDispatch = *noDispatch
	runnerDSN, runnerCredential, err := runnerConnection(*runnerDSNEnv, false)
	if err != nil {
		return err
	}
	defer runnerCredential.Close()
	cfg.RunnerDSN = runnerDSN
	cfg.DispatchVia = *dispatchVia
	cfg.TrustProvider = *trustProvider
	cfg.WorkerUser = *workerUser
	cfg.LaunchLimit = *launchLimit
	cfg.OnlyMission = domain.MissionID(*onlyMission)
	if cfg.DispatchVia == sched.DispatchRegistry {
		// NEG-11: registry mode runs only the pinned stdin fake. --trust-provider
		// alone is never enough: the resolved file must hash to the pin here,
		// and the dispatcher re-checks it at every launch.
		got, err := dispatch.FileSHA256(dp.Path)
		if err != nil {
			return fmt.Errorf("refusing --dispatch-via=registry: provider pin: %w", err)
		}
		if got != dispatch.StdinFakeSHA256 {
			return fmt.Errorf("refusing --dispatch-via=registry: provider %s has sha256 %s, not the pinned stdin fake (goodqa#15373 NEG-11)", dp.Path, got)
		}
		reg := dispatch.NewRegistry()
		if err := reg.Register(dispatch.ClaudeCode{PinSHA256: dispatch.StdinFakeSHA256}); err != nil {
			return err
		}
		cfg.Registry = reg
	}
	// Verified descendant cleanup after every worker exit before settling:
	// on by default for a real provider (goodqa#15976 AC-5), otherwise only
	// with FABRIC_WORKER_EXIT_CLEANUP=1 (exact). Opt-in for the fakes: on
	// Darwin the Stop signals the exited leader's process group, which
	// widens a PGID-reuse window until the host profile closes it.
	cfg.ExitCleanup = daemonExitCleanup(realProvider)
	// goodqa#15087 AC-6: the same launch the AC-6 (f) live check runs. It
	// must stay dp.Launch: the operator's name, re-resolved per launch (N1;
	// TestDaemonLaunchComesFromDaemonProvider guards this line).
	cfg.Launch = dp.Launch
	// goodqa#15373 AC-2: every configuration refusal before the store opens,
	// so a bad flag exits once and never crash-loops the unit.
	if err := sched.CheckConfig(cfg, ln); err != nil {
		return err
	}
	return withStoreService(func(ctx context.Context, st *store.Store) error {
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

// resolveStartProvider resolves the provider when fabd starts. It refuses
// with a named cause when the name does not resolve to a file (a typo, a
// PATH without it, an install mid-update): fabd then does not start,
// rather than start with the provider control skipped (goodqa#15161 R1).
func resolveStartProvider(provider string) (string, error) {
	p := providerPath(provider)
	if p == "" {
		return "", fmt.Errorf("provider %q cannot be resolved to a file; refusing to start fabd (the provider control would have nothing to run)", provider)
	}
	return p, nil
}

// daemonProvider is the provider fabd works with. Path is the file the
// operator's name resolved to at start: the containment control runs it and
// the Darwin profile reads it back (review R1). Launch keeps the name and
// re-resolves it at every launch (N1), so an update is followed.
type daemonProvider struct {
	Path   string
	Launch claude.Launch
	// SealRoot holds the sealed copies a fake-only start launches
	// (goodqa#15976 F1): <work root>/pinned-providers, beside the schema.
	SealRoot string
}

func newDaemonProvider(operator, schemaPath, hooksDir string) (daemonProvider, error) {
	p, err := resolveStartProvider(operator)
	if err != nil {
		return daemonProvider{}, err
	}
	return daemonProvider{Path: p, Launch: productionLaunch(operator, schemaPath, hooksDir), SealRoot: sealRootFor(schemaPath)}, nil
}

// productionLaunch is the launch fabd hands the scheduler. Binary keeps
// the operator's name and Resolve maps it to a file at every launch, so a
// provider update that repoints the symlink and prunes the old version is
// followed (goodqa#15161 N1), the launch never execs through a symlink in
// a directory the Darwin profile denies (review C), and a name that does
// not resolve refuses the launch (review R3: this wiring is tested).
func productionLaunch(provider, schemaPath, hooksDir string) claude.Launch {
	l := claude.ProductionLaunch(provider, schemaPath, hooksDir)
	l.Resolve = providerPath
	return l
}

// resolveRung reads the tier table file and returns (model, effort, sha256 of the file).
func resolveRung(path, rung string) (string, string, string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", "", "", err
	}
	// goodqa#15178: invalid UTF-8 decodes to U+FFFD, so two different
	// tables could share a digest and a model name could be mangled.
	if !utf8.Valid(b) {
		return "", "", "", fmt.Errorf("tier table %s is not valid UTF-8", path)
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
	sha, err := domainSHA(b)
	if err != nil {
		return "", "", "", err
	}
	return r.Models["claude"], r.Effort, sha, nil
}

// domainSHA digests the tier-table bytes as a JSON string. encoding/json
// escapes them; hand-escaping only quotes made a multi-line file invalid
// JSON and an empty digest, which the grant refuses (goodqa#15172).
// HTML escaping stays off so a table that the old code digested correctly
// (no newline, tab or backslash) keeps its digest.
func domainSHA(b []byte) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(string(b)); err != nil {
		return "", err
	}
	base, err := domain.EffectBase("tier-table", json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n")))
	if err != nil || len(base) < 16 {
		return "", fmt.Errorf("tier table digest: %v", err)
	}
	return base[:16], nil
}
