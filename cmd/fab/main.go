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
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"

	"fabric/internal/domain"
	"fabric/internal/host"
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
	case "mission":
		err = missionMain(os.Args[2:])
	case "admit", "release", "wake":
		err = simpleCommand(os.Args[1], os.Args[2:])
	case "status":
		err = statusMain(os.Args[2:])
	case "asks":
		err = asksMain(os.Args[2:])
	case "import":
		err = importMain(os.Args[2:])
	case "expire-leases":
		err = expireMain(os.Args[2:])
	case "host":
		err = hostMain(os.Args[2:])
	case "migrate":
		err = withStore(func(ctx context.Context, st *store.Store) error { return st.Migrate(ctx) })
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "fab:", err)
		var ref *store.Refusal
		if errors.As(err, &ref) {
			os.Exit(3)
		}
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: fab <command> [flags]
  migrate                              apply migrations
  mission new|list                     register / list missions
  admit|release|wake --mission M [--token T] --key K
  status                               missions, slots, open asks
  asks [--state open]                  the inbox
  import --slots F --queue F           one-off #14882 import (shadow)
  expire-leases [--limit N]            bounded lease sweep
  host controls --scratch DIR          run this host's negative controls
  __sandbox --rw DIR ... -- argv       (linux) apply Landlock then exec`)
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
		return errors.New("mission new|list")
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
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return withStore(func(ctx context.Context, st *store.Store) error {
			err := st.CreateMissionSpec(ctx, store.MissionSpec{ID: domain.MissionID(*id), Name: *name, Ticket: *ticket, Repo: *repo,
				Branch: *branch, Priority: *prio, HostNeeds: *needs, Source: *source})
			if err == nil {
				fmt.Println("mission", *id, "registered (IDLE)")
			}
			return err
		})
	case "list":
		return withStore(func(ctx context.Context, st *store.Store) error { return printMissions(ctx, st) })
	}
	return errors.New("mission new|list")
}

func printMissions(ctx context.Context, st *store.Store) error {
	rows, err := st.ListMissions(ctx)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "MISSION\tPRIO\tEXEC\tCAND\tDELIV\tREV/FENCE/GEN\tTICKET\tNAME")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\t%d/%d/%d\t%s\t%s\n", r.ID, r.Priority, r.Execution, r.Candidate, r.Delivery,
			r.Version.IntentRevision, r.Version.AttemptFence, r.Version.CandidateGeneration, r.Ticket, r.Name)
	}
	return w.Flush()
}

func simpleCommand(name string, args []string) error {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	mission := fs.String("mission", "", "mission id")
	token := fs.String("token", "", "wake token (wake only)")
	key := fs.String("key", "", "idempotency key")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cmd := domain.Command{Mission: domain.MissionID(*mission), Token: *token}
	switch name {
	case "admit":
		cmd.Kind = domain.CmdAdmitMission
	case "release":
		cmd.Kind = domain.CmdRelease
	case "wake":
		cmd.Kind = domain.CmdWake
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

type slotsFile struct {
	Source      string `json:"source"`
	QueueTicket string `json:"queue_ticket"`
	Rules       struct {
		MaxActiveOwners int `json:"max_active_owners"`
	} `json:"rules"`
	Slots []struct {
		Slot   int    `json:"slot"`
		Name   string `json:"name"`
		Ticket string `json:"ticket"`
	} `json:"slots"`
	QueuedInOrder []json.RawMessage `json:"queued_in_order"`
}

type queueFile struct {
	Items []struct {
		Ask            string `json:"ask"`
		Need           string `json:"need"`
		DefaultApplied string `json:"default_applied"`
		Status         string `json:"status"`
	} `json:"items"`
}

func ticketRef(raw, fallback string) string {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "GoodQA ") {
		return raw
	}
	if strings.HasPrefix(raw, "#") {
		return "GoodQA goodqa" + raw
	}
	return fallback
}

func importMain(args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	slotsPath := fs.String("slots", "", "active-slots.json")
	queuePath := fs.String("queue", "", "user-decisions-queue.json")
	mainWAL := fs.String("wal", "", "absolute path of the main WAL the ask ids come from (for wal: sources)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *slotsPath == "" || *queuePath == "" || *mainWAL == "" {
		return errors.New("--slots, --queue and --wal are required")
	}
	var sf slotsFile
	var qf queueFile
	if err := readJSON(*slotsPath, &sf); err != nil {
		return err
	}
	if err := readJSON(*queuePath, &qf); err != nil {
		return err
	}
	return withStore(func(ctx context.Context, st *store.Store) error {
		if sf.Rules.MaxActiveOwners > 0 {
			if err := st.SetCap(ctx, sf.Rules.MaxActiveOwners); err != nil {
				return err
			}
		}
		fallback := sf.QueueTicket
		if fallback == "" {
			fallback = "GoodQA goodqa#14882"
		}
		src := "file:" + *slotsPath
		admitted, queued := 0, 0
		for _, s := range sf.Slots {
			id := domain.MissionID(fmt.Sprintf("m_slot%d", s.Slot))
			if err := st.CreateMissionSpec(ctx, store.MissionSpec{ID: id, Name: s.Name, Ticket: ticketRef(s.Ticket, fallback), Priority: s.Slot, Source: src}); err != nil {
				return err
			}
			_, err := execute(ctx, st, "import", "import:admit:"+string(id), domain.Command{Kind: domain.CmdAdmitMission, Mission: id})
			if err != nil && !errors.As(err, new(*store.Refusal)) {
				return err
			}
			if err == nil {
				admitted++
			}
		}
		for i, raw := range sf.QueuedInOrder {
			name, ticket := queuedItem(raw)
			id := domain.MissionID(fmt.Sprintf("m_q%02d", i+1))
			if err := st.CreateMissionSpec(ctx, store.MissionSpec{ID: id, Name: name, Ticket: ticketRef(ticket, fallback), Priority: 10 + i, Source: src}); err != nil {
				return err
			}
			queued++
		}
		asks := 0
		for i, it := range qf.Items {
			if strings.TrimSpace(it.Need) == "" {
				continue
			}
			// An item that names a real main-WAL ask id is sourced from that
			// entry; anything else is sourced from the file itself (R12).
			a := store.Ask{ID: fmt.Sprintf("q_item%02d", i+1), Kind: "decision", Question: it.Need, Recommended: it.DefaultApplied, Source: "file:" + *queuePath}
			if walID.MatchString(it.Ask) {
				a.ID = it.Ask
				a.Source = "wal:" + *mainWAL + "#" + it.Ask
			}
			if err := st.CreateAsk(ctx, a); err != nil {
				return err
			}
			asks++
		}
		fmt.Printf("import: cap=%d, slots admitted=%d, queued=%d, asks=%d (shadow: no effects posted)\n", sf.Rules.MaxActiveOwners, admitted, queued, asks)
		return nil
	})
}

var walID = regexp.MustCompile(`^w[0-9a-f]{11}$`)

func queuedItem(raw json.RawMessage) (name, ticket string) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, firstTicket(s)
	}
	var o struct {
		Name   string `json:"name"`
		Item   string `json:"item"`
		Ticket string `json:"ticket"`
	}
	if json.Unmarshal(raw, &o) == nil {
		n := o.Name
		if n == "" {
			n = o.Item
		}
		if o.Ticket != "" {
			return n, o.Ticket
		}
		return n, firstTicket(n)
	}
	return string(raw), ""
}

func firstTicket(s string) string {
	if i := strings.Index(s, "#"); i >= 0 {
		j := i + 1
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		if j > i+1 {
			return s[i:j]
		}
	}
	return ""
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(b) > 4<<20 {
		return fmt.Errorf("%s: larger than 4 MiB", path)
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	return dec.Decode(v)
}

// --- host ---

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
