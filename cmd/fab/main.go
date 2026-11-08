// Command fab is the fabric's CLI (DESIGN.md 3.2). Until fabd exists it
// talks to the store directly (FABRIC_DSN). Every write carries an
// idempotency key and is recorded as a pending intent on disk before the
// call, so a lost response is retried with the same key, never a new one.
package main

import (
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
	"text/tabwriter"
	"time"

	"fabric/internal/domain"
	"fabric/internal/host"
	"fabric/internal/importer"
	"fabric/internal/store"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "__sandbox":
		err = sandboxMain(os.Args[2:])
	case "provider":
		err = providerMain(os.Args[2:])
	case "mission":
		err = missionMain(os.Args[2:])
	case "admit", "release", "wake", "done", "unpause":
		err = simpleCommand(os.Args[1], os.Args[2:])
	case "live-test":
		err = liveTestMain(os.Args[2:])
	case "status":
		err = statusMain(os.Args[2:])
	case "asks":
		err = asksMain(os.Args[2:])
	case "ask":
		err = askMain(os.Args[2:])
	case "import":
		err = importMain(os.Args[2:])
	case "expire-leases":
		err = expireMain(os.Args[2:])
	case "reconcile":
		err = reconcileMain(os.Args[2:])
	case "host":
		err = hostMain(os.Args[2:])
	case "attempt":
		err = attemptMain(os.Args[2:])
	case "effect":
		err = effectMain(os.Args[2:])
	case "review":
		err = reviewMain(os.Args[2:])
	case "job":
		err = jobMain(os.Args[2:])
	case "daemon":
		err = daemonMain(os.Args[2:])
	case "db":
		err = dbMain(os.Args[2:])
	case "metrics":
		err = metricsMain(os.Args[2:])
	case "serve-metrics":
		err = serveMetricsMain(os.Args[2:])
	case "ladder":
		err = ladderMain(os.Args[2:])
	case "tui":
		err = tuiMain(os.Args[2:])
	case "version":
		err = versionMain(os.Args[2:])
	case "migrate":
		err = withStore(func(ctx context.Context, st *store.Store) error { return st.Migrate(ctx) })
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "fab:", err)
		os.Exit(exitCode(err))
	}
}

// exitCode: 3 is a refusal (nothing changed), 1 an error of unknown effect.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ref *store.Refusal
	var ue usageError
	var rr reconcileRefused
	if errors.As(err, &ref) || errors.As(err, &ue) || errors.As(err, &rr) {
		return 3
	}
	return 1
}

// usageError is a refused invocation, exit 3 like a refusal: a required
// flag is missing or holds an invalid value (goodqa#15087).
type usageError struct{ error }

func (e usageError) Unwrap() error { return e.error }

func usage() {
	fmt.Fprintln(os.Stderr, `usage: fab <command> [flags]
  migrate                              apply migrations
  db roles [--prefix P --app-password-file F --runner-password-file F]   least-privilege roles (after migrate)
  db check-runner [--dsn-env E]        verify the runner role cannot act as the scheduler
  db capacity --resource R --unit U --capacity N   set a dispatch-gate resource
  mission new|list                     register / list missions
  admit|release|wake --mission M [--token T] --key K
  unpause --mission M --reason R --key K   (operator: FABRIC_ACTOR=op:<name>)
  admit of a parked mission also needs FABRIC_ACTOR=op:<name> and --reason
  live-test --mission M --commit C --generation G --host H --target URL --receipt R --key K
                                       record a live_test receipt for the deployed commit (AC-6)
  done --mission M --key K             close a mission; refused without a live_test receipt (AC-6)
  status                               missions, slots, open asks
  metrics [--timeout D]                health metrics (Prometheus text format, read-only)
  serve-metrics [--listen 127.0.0.1:9464] [--allow-remote] [--timeout D]   GET /metrics; loopback only by default
  tui [--interval 2s] [--timeout 1s] [--no-color] [--keys on|off]   read-only terminal view (fab tui -h: keys)
  version                              build revision
  ladder status --state F --policy F [--json]   model ladder from an escalation.py state file (read-only)
  asks [--state open]                  the inbox
  ask new --id I --question Q --source S [--mission M --kind K --option O ...]
  ask answer I --answer A --source S   answer; wakes the mission waiting on ask:I
  ask withdraw I --reason R | show I | repair
  mission show M                       snapshot and event log (JSON)
  mission run --provider anthropic --grant F   run a native-loop mission (API key from the grant's key_command)
                                       FABRIC_REVIEW_GATE=1 gates every tool (needs FABRIC_REVIEW_DSN); default off.
                                       FABRIC_TOOL_EXIT_CLEANUP=1 verifies descendant cleanup after every tool exit; default off.
                                       (fab daemon: FABRIC_WORKER_EXIT_CLEANUP=1 does the same for each worker before settling; default off.)
                                       grant review_gate.mode: shadow (default, no execution), evaluate (no execution), enforce
  review list [--state S] | show HASH  inspect request metadata (never args or prompts)
  review approve --hash H --reason R --expires-in D | review reject --hash H --reason R
                                       separate human operator; requires FABRIC_ACTOR=op:name
                                       approval wakes the lease-protected native runner; rejection also resumes it with denial
  provider capture --grant F --out DIR [--scenarios text,tool,thinking,badauth,badrequest]   record real API streams
  import --slots F --queue F           one-off #14882 import (shadow)
  expire-leases [--limit N]            bounded lease sweep
  reconcile [--host H]                 one expire + quarantine pass for this host (no fabd); exit 3 while a fabd holds the host
  host controls --scratch DIR          run this host's negative controls
  __sandbox --env-keep NAMES --rw DIR ... -- argv (linux) apply Landlock, exec with only NAMES`)
}

func withStore(f func(context.Context, *store.Store) error) error {
	dsn := os.Getenv("FABRIC_DSN")
	if dsn == "" {
		return errors.New("FABRIC_DSN is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer st.Close()
	return f(ctx, st)
}

func fabricHome() string {
	if h := os.Getenv("FABRIC_HOME"); h != "" {
		return h
	}
	return filepath.Join(os.Getenv("HOME"), ".fabric")
}

// execute runs one command under its key with the pending-intent file
// written first (lost-response protocol, DESIGN.md 3.1).
func execute(ctx context.Context, st *store.Store, actor, key string, cmd domain.Command) (store.Response, error) {
	if key == "" {
		return store.Response{}, errors.New("--key is required: the client mints the idempotency key and never changes it on retry")
	}
	pending := filepath.Join(fabricHome(), "pending")
	if err := os.MkdirAll(pending, 0o700); err != nil {
		return store.Response{}, err
	}
	body, _ := json.Marshal(map[string]any{"actor": actor, "key": key, "command": cmd, "at": time.Now().UTC()})
	pfile := filepath.Join(pending, key+".json")
	if err := os.WriteFile(pfile, body, 0o600); err != nil {
		return store.Response{}, err
	}
	resp, err := st.Execute(ctx, domain.ActorID(actor), domain.CommandID(key), cmd)
	if err == nil || errors.As(err, new(*store.Refusal)) {
		os.Remove(pfile) // a stored answer exists; the intent is settled either way
	}
	return resp, err
}

func actor() string {
	if a := os.Getenv("FABRIC_ACTOR"); a != "" {
		return a
	}
	return "chat"
}

func missionMain(args []string) error {
	if len(args) == 0 {
		return errors.New("mission new|list|show")
	}
	switch args[0] {
	case "new":
		fs := flag.NewFlagSet("mission new", flag.ContinueOnError)
		id := fs.String("id", "", "mission id (m_...)")
		name := fs.String("name", "", "short name")
		ticket := fs.String("ticket", "", "GoodQA ticket, e.g. 'GoodQA goodqa#14791' (required, R5)")
		repo := fs.String("repo", "", "repository")
		branch := fs.String("branch", "", "branch")
		prio := fs.Int("priority", 100, "queue position (lower first)")
		needs := fs.String("needs", "", "host label needed, e.g. heavy")
		source := fs.String("source", "", "provenance of this mission (R12 grammar)")
		provider := fs.String("provider", "", "claude-code (fabd runs it on the Claude Code binary) or anthropic-api (only `fab mission run --grant` runs it); required, never changes")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		prov, err := domain.ParseProvider(*provider)
		if err != nil {
			return usageError{fmt.Errorf("mission new: --provider: %w", err)}
		}
		return withStore(func(ctx context.Context, st *store.Store) error {
			err := st.CreateMissionSpec(ctx, store.MissionSpec{ID: domain.MissionID(*id), Name: *name, Ticket: *ticket, Repo: *repo,
				Branch: *branch, Priority: *prio, HostNeeds: *needs, Source: *source, Provider: prov})
			if err == nil {
				fmt.Println("mission", *id, "registered (IDLE)")
			}
			return err
		})
	case "list":
		return withStore(func(ctx context.Context, st *store.Store) error { return printMissions(ctx, st) })
	case "show":
		return missionShow(args[1:])
	case "run":
		return nativeRunMain(args[1:])
	}
	return errors.New("mission new|list|show|run")
}

func printMissions(ctx context.Context, st *store.Store) error {
	rows, err := st.ListMissions(ctx)
	if err != nil {
		return err
	}
	return writeMissions(os.Stdout, rows)
}

// writeMissions is the mission table of `fab status` (goodqa#15087: with
// each mission's provider, and the awaiting state of an admitted
// anthropic-api mission that only `fab mission run --grant` starts).
func writeMissions(out io.Writer, rows []store.MissionRow) error {
	w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "MISSION\tPROVIDER\tPRIO\tEXEC\tCAND\tDELIV\tREV/FENCE/GEN\tTICKET\tNAME")
	for _, r := range rows {
		exec := string(r.Execution)
		if r.AwaitingGrant {
			exec += " (awaiting mission run --grant)"
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\t%d/%d/%d\t%s\t%s\n", r.ID, r.Provider, r.Priority, exec, r.Candidate, r.Delivery,
			r.Version.IntentRevision, r.Version.AttemptFence, r.Version.CandidateGeneration, r.Ticket, r.Name)
	}
	return w.Flush()
}

func simpleCommand(name string, args []string) error {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	mission := fs.String("mission", "", "mission id")
	token := fs.String("token", "", "wake token (wake only)")
	reason := fs.String("reason", "", "why (required for unpause and for admitting a parked mission)")
	key := fs.String("key", "", "idempotency key")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cmd := domain.Command{Mission: domain.MissionID(*mission), Token: *token, Reason: *reason}
	switch name {
	case "admit":
		cmd.Kind = domain.CmdAdmitMission
	case "release":
		cmd.Kind = domain.CmdRelease
	case "wake":
		cmd.Kind = domain.CmdWake
	case "done":
		cmd.Kind = domain.CmdCompleteMission
	case "unpause":
		cmd.Kind = domain.CmdUnpauseMission
	}
	return withStore(func(ctx context.Context, st *store.Store) error {
		resp, err := execute(ctx, st, actor(), *key, cmd)
		if err != nil {
			return err
		}
		fmt.Printf("%s %s: %s (%d events, replayed=%v, version %+v)\n", name, *mission, resp.Status, len(resp.Events), resp.Replayed, resp.Version)
		return nil
	})
}

func statusMain(args []string) error {
	return withStore(func(ctx context.Context, st *store.Store) error {
		held, err := st.SlotsHeld(ctx)
		if err != nil {
			return err
		}
		var cap int
		if err := st.Pool().QueryRow(ctx, "SELECT cap FROM slots WHERE id = 1").Scan(&cap); err != nil {
			return err
		}
		fmt.Printf("slots: %d of %d held\n", held, cap)
		if err := printMissions(ctx, st); err != nil {
			return err
		}
		asks, err := st.ListAsks(ctx, "open")
		if err != nil {
			return err
		}
		fmt.Printf("open asks: %d\n", len(asks))
		for _, a := range asks {
			fmt.Printf("  %s [%s] %s\n      default: %s\n      source: %s\n", a.ID, a.Kind, a.Question, a.Recommended, a.Source)
		}
		return nil
	})
}

func asksMain(args []string) error {
	fs := flag.NewFlagSet("asks", flag.ContinueOnError)
	state := fs.String("state", "open", "open|answered|withdrawn|'' for all")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return withStore(func(ctx context.Context, st *store.Store) error {
		asks, err := st.ListAsks(ctx, *state)
		if err != nil {
			return err
		}
		for _, a := range asks {
			fmt.Printf("%s\t%s\t%s\t%s\n", a.ID, a.State, a.Kind, a.Question)
		}
		return nil
	})
}

func expireMain(args []string) error {
	fs := flag.NewFlagSet("expire-leases", flag.ContinueOnError)
	limit := fs.Int("limit", 50, "page size")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return withStore(func(ctx context.Context, st *store.Store) error {
		exp, err := st.ExpireLeases(ctx, *limit)
		for _, e := range exp {
			fmt.Printf("expired %s (mission %s, fence %d) -> UNKNOWN\n", e.Attempt, e.Mission, e.Fence)
		}
		return err
	})
}

// --- import of the #14882 queue files (DESIGN.md 4.1), shadow only ---

func importMain(args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	var o importer.Options
	fs.StringVar(&o.SlotsPath, "slots", "", "active-slots.json")
	fs.StringVar(&o.QueuePath, "queue", "", "user-decisions-queue.json")
	fs.StringVar(&o.MainWAL, "wal", "", "absolute path of the main WAL the ask ids come from (for wal: sources)")
	fs.BoolVar(&o.ApplyCap, "apply-cap", false, "set the global admission cap from the slots file (off: the file only proposes it)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	sf, qf, err := importer.Load(o)
	if err != nil {
		return err
	}
	return withStore(func(ctx context.Context, st *store.Store) error {
		exec := func(ctx context.Context, actor, key string, cmd domain.Command) (store.Response, error) {
			return execute(ctx, st, actor, key, cmd)
		}
		res, err := importer.Run(ctx, st, exec, o, sf, qf)
		fmt.Println("import:", res)
		return err
	})
}

func hostMain(args []string) error {
	if len(args) == 0 || args[0] != "controls" {
		return errors.New("host controls --scratch DIR")
	}
	fs := flag.NewFlagSet("host controls", flag.ContinueOnError)
	scratch := fs.String("scratch", "", "scratch directory for the controls")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *scratch == "" {
		return errors.New("--scratch is required")
	}
	p, err := profileForThisHost()
	if err != nil {
		return err
	}
	results, err := p.NegativeControls(*scratch)
	if err != nil {
		return err
	}
	failed := 0
	for _, r := range results {
		status := "PASS"
		if !r.Passed {
			status = "FAIL"
			failed++
		}
		fmt.Printf("%s\t%s\t%s\n", status, r.Name, r.Detail)
	}
	fmt.Printf("%s: %d/%d controls passed\n", p.Name(), len(results)-failed, len(results))
	if failed > 0 {
		return fmt.Errorf("%d controls failed; this host may not advertise the profile", failed)
	}
	return nil
}

func profileForThisHost() (host.Profile, error) {
	switch runtime.GOOS {
	case "linux":
		self, err := os.Executable()
		if err != nil {
			return nil, err
		}
		return newLinuxProfile(self), nil
	case "darwin":
		sb, err := exec.LookPath("sandbox-exec")
		if err != nil {
			return nil, errors.New("sandbox-exec not found")
		}
		return newDarwinProfile(sb), nil
	}
	return nil, errors.New("no profile for " + runtime.GOOS)
}

// liveTestMain records a live_test receipt (AC-6): the deployed commit, the
// live target it ran against and where the receipt is. It is not a pass for
// a vacuous run; the reducer refuses one bound to any other commit.
func liveTestMain(args []string) error {
	fs := flag.NewFlagSet("live-test", flag.ContinueOnError)
	mission := fs.String("mission", "", "mission id")
	commit := fs.String("commit", "", "deployed commit the live test ran against")
	generation := fs.Int64("generation", 0, "candidate generation that was deployed")
	host := fs.String("host", "", "host the live test ran on")
	target := fs.String("target", "", "live target (URL or service identity)")
	receipt := fs.String("receipt", "", "receipt reference (log, WAL REALLY_TESTED id or artifact)")
	vacuous := fs.Bool("vacuous", false, "the run passed nothing (recorded, never a pass)")
	key := fs.String("key", "", "idempotency key")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cmd := domain.Command{Kind: domain.CmdRecordLiveTest, Mission: domain.MissionID(*mission), Head: *commit, Generation: *generation,
		Host: domain.HostID(*host), Target: *target, Receipt: *receipt, Vacuous: *vacuous}
	return withStore(func(ctx context.Context, st *store.Store) error {
		resp, err := execute(ctx, st, actor(), *key, cmd)
		if err != nil {
			return err
		}
		fmt.Printf("live-test %s: %s (%d events, replayed=%v)\n", *mission, resp.Status, len(resp.Events), resp.Replayed)
		return nil
	})
}
