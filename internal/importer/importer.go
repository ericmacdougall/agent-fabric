// Package importer is the one-off shadow import of the #14882 queue files
// (DESIGN.md 4.1): the active slots become admitted missions, the queue
// becomes missions in order, and the decisions queue becomes asks. It posts
// no effects. Every write is keyed, so a second run replays to the same state.
package importer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"syscall"

	"fabric/internal/domain"
	"fabric/internal/store"
)

// MaxFileBytes bounds each input file: the files are untrusted JSON.
const MaxFileBytes = 4 << 20

// DefaultQueueTicket is the queue ticket when the slots file names none.
const DefaultQueueTicket = "GoodQA goodqa#14882"

// SlotsFile is active-slots.json.
type SlotsFile struct {
	Source      string `json:"source"`
	QueueTicket string `json:"queue_ticket"`
	Rules       struct {
		MaxActiveOwners int `json:"max_active_owners"`
	} `json:"rules"`
	Slots []struct {
		Slot   int    `json:"slot"`
		Name   string `json:"name"`
		Ticket string `json:"ticket"`
		// Provider: absent or claude-code; anything else refuses the
		// import (goodqa#15087, main WAL wbdc1146b5e3).
		Provider *string `json:"provider"`
	} `json:"slots"`
	QueuedInOrder []json.RawMessage `json:"queued_in_order"`
}

// QueueFile is user-decisions-queue.json.
type QueueFile struct {
	Items []struct {
		Ask            string `json:"ask"`
		Need           string `json:"need"`
		DefaultApplied string `json:"default_applied"`
		Status         string `json:"status"`
	} `json:"items"`
}

// Executor runs one keyed command; the CLI passes its pending-intent wrapper.
type Executor func(ctx context.Context, actor, key string, cmd domain.Command) (store.Response, error)

// Options name the inputs. The paths become file: sources (R12) and must not
// contain spaces; MainWAL is the WAL that wal: ask sources point into.
type Options struct {
	SlotsPath string
	QueuePath string
	MainWAL   string
	// ApplyCap sets the global admission cap from the file. Off by default:
	// the file is untrusted input and the cap bounds every mission (F5).
	ApplyCap bool
}

// Result counts what one run did. A second run over the same files reports
// zero Admitted and the same total under Replayed.
type Result struct {
	Cap       int      // the admission cap in effect
	FileCap   int      // the cap the slots file proposes (applied only with ApplyCap)
	Admitted  int      // admitted by this run
	Replayed  int      // admission already decided under its key, admitted or refused (served from the store)
	Refused   int      // admission refused by this run, e.g. over the cap
	Queued    int      // queued missions registered by this run
	Asks      int      // asks registered by this run
	Unchanged int      // missions and asks already registered with the same text
	Changed   []string // missions and asks whose stored text differs from the files; kept, not overwritten
	Skipped   int      // queue items with no need text
}

func (r Result) String() string {
	capNote := ""
	if r.FileCap > 0 && r.FileCap != r.Cap {
		capNote = fmt.Sprintf(" (file cap %d not applied; --apply-cap applies it)", r.FileCap)
	}
	changed := ""
	if len(r.Changed) > 0 {
		changed = " " + strings.Join(r.Changed, ",")
	}
	return fmt.Sprintf("cap=%d%s, slots admitted=%d replayed=%d refused=%d, queued=%d, asks=%d, unchanged=%d, changed=%d%s, skipped=%d (shadow: no effects posted)",
		r.Cap, capNote, r.Admitted, r.Replayed, r.Refused, r.Queued, r.Asks, r.Unchanged, len(r.Changed), changed, r.Skipped)
}

// Load reads and decodes both files, refusing anything over MaxFileBytes.
func Load(o Options) (SlotsFile, QueueFile, error) {
	var sf SlotsFile
	var qf QueueFile
	if o.SlotsPath == "" || o.QueuePath == "" || o.MainWAL == "" {
		return sf, qf, errors.New("the slots file, the queue file and the main WAL path are all required")
	}
	for _, p := range []string{o.SlotsPath, o.QueuePath, o.MainWAL} {
		if strings.ContainsAny(p, " \t\n") {
			return sf, qf, fmt.Errorf("path %q: a provenance source cannot contain whitespace", p)
		}
	}
	if err := readJSON(o.SlotsPath, &sf); err != nil {
		return sf, qf, err
	}
	if err := readJSON(o.QueuePath, &qf); err != nil {
		return sf, qf, err
	}
	return sf, qf, nil
}

func readJSON(path string, v any) error {
	// Review F6: O_NONBLOCK so a FIFO cannot block the open waiting for a
	// writer, then the mode of the opened file itself (no stat-then-open race).
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s: not a regular file (%s)", path, fi.Mode().Type())
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err != nil {
		return err
	}
	if len(b) > MaxFileBytes {
		return fmt.Errorf("%s: larger than %d bytes", path, MaxFileBytes)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// Run applies the files to the store. exec defaults to st.Execute.
func Run(ctx context.Context, st *store.Store, exec Executor, o Options, sf SlotsFile, qf QueueFile) (Result, error) {
	if exec == nil {
		exec = func(ctx context.Context, actor, key string, cmd domain.Command) (store.Response, error) {
			return st.Execute(ctx, domain.ActorID(actor), domain.CommandID(key), cmd)
		}
	}
	res := Result{FileCap: sf.Rules.MaxActiveOwners}
	if o.ApplyCap && sf.Rules.MaxActiveOwners > 0 {
		if err := st.SetCap(ctx, sf.Rules.MaxActiveOwners); err != nil {
			return res, err
		}
	}
	c, err := st.SlotCap(ctx)
	if err != nil {
		return res, err
	}
	res.Cap = c
	// note counts one idempotent registration; it reports whether it was new.
	note := func(id string, e store.Ensured) bool {
		switch e {
		case store.Unchanged:
			res.Unchanged++
		case store.Differs:
			res.Changed = append(res.Changed, id)
		}
		return e == store.Created
	}
	fallback := sf.QueueTicket
	if fallback == "" {
		fallback = DefaultQueueTicket
	}
	src := "file:" + o.SlotsPath
	seen := map[int]bool{}
	for _, s := range sf.Slots {
		if s.Slot < 1 || seen[s.Slot] {
			return res, fmt.Errorf("slot number %d is missing or repeated", s.Slot)
		}
		seen[s.Slot] = true
		id := domain.MissionID(fmt.Sprintf("m_slot%d", s.Slot))
		prov, err := importProvider(string(id), s.Provider)
		if err != nil {
			return res, err
		}
		e, err := st.EnsureMissionSpec(ctx, store.MissionSpec{ID: id, Name: s.Name, Ticket: TicketRef(s.Ticket, fallback), Priority: s.Slot, Source: src, Provider: prov})
		if err != nil {
			return res, err
		}
		note(string(id), e)
		resp, err := exec(ctx, "import", "import:admit:"+string(id), domain.Command{Kind: domain.CmdAdmitMission, Mission: id})
		var ref *store.Refusal
		switch {
		case errors.As(err, &ref) && ref.Response.Replayed:
			res.Replayed++
		case errors.As(err, &ref):
			res.Refused++
		case err != nil:
			return res, err
		case resp.Replayed:
			res.Replayed++
		default:
			res.Admitted++
		}
	}
	for i, raw := range sf.QueuedInOrder {
		name, ticket := QueuedItem(raw)
		id := domain.MissionID(fmt.Sprintf("m_q%02d", i+1))
		provider, err := queuedProvider(string(id), raw)
		if err != nil {
			return res, err
		}
		prov, err := importProvider(string(id), provider)
		if err != nil {
			return res, err
		}
		e, err := st.EnsureMissionSpec(ctx, store.MissionSpec{ID: id, Name: name, Ticket: TicketRef(ticket, fallback), Priority: 10 + i, Source: src, Provider: prov})
		if err != nil {
			return res, err
		}
		if note(string(id), e) {
			res.Queued++
		}
	}
	for i, it := range qf.Items {
		if strings.TrimSpace(it.Need) == "" {
			res.Skipped++
			continue
		}
		// An item that names a real main-WAL ask id is sourced from that
		// entry; anything else (including "none") is sourced from the file.
		a := store.Ask{ID: fmt.Sprintf("q_item%02d", i+1), Kind: "decision", Question: it.Need, Recommended: it.DefaultApplied, Source: "file:" + o.QueuePath}
		if walID.MatchString(it.Ask) {
			a.ID = it.Ask
			a.Source = "wal:" + o.MainWAL + "#" + it.Ask
		}
		e, err := st.EnsureAsk(ctx, a)
		if err != nil {
			return res, err
		}
		if note(a.ID, e) {
			res.Asks++
		}
	}
	sort.Strings(res.Changed)
	return res, nil
}

var walID = regexp.MustCompile(`^w[0-9a-f]{11}$`)

// TicketRef normalises "GoodQA p#N" or "#N"; anything else gets fallback.
func TicketRef(raw, fallback string) string {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "GoodQA ") {
		return raw
	}
	if strings.HasPrefix(raw, "#") {
		return "GoodQA goodqa" + raw
	}
	return fallback
}

// queuedProvider reads a queued row's provider field. A legacy row is a JSON
// string and has no provider; an object's provider must be a string or null.
// Anything else is refused, never silently read as an absent provider (which
// would default the row to claude-code).
func queuedProvider(row string, raw json.RawMessage) (*string, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed[0] != '{' {
		var legacy string
		if trimmed == "" || trimmed[0] != '"' || json.Unmarshal(raw, &legacy) != nil {
			return nil, fmt.Errorf("%w: import row %s legacy entry must be a JSON string", domain.ErrInvalidProvider, row)
		}
		return nil, nil
	}
	var o struct {
		Provider json.RawMessage `json:"provider"`
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		return nil, fmt.Errorf("%w: import row %s is malformed JSON", domain.ErrInvalidProvider, row)
	}
	if len(o.Provider) == 0 || string(o.Provider) == "null" {
		return nil, nil
	}
	var provider string
	if err := json.Unmarshal(o.Provider, &provider); err != nil {
		return nil, fmt.Errorf("%w: import row %s provider must be a string", domain.ErrInvalidProvider, row)
	}
	return &provider, nil
}

// QueuedItem reads a queue entry that is either a string or an object with
// name/item and ticket.
// importProvider: an import row is a claude-code mission. A row with no
// provider field, or with provider claude-code, is accepted; any other
// value (anthropic-api included) refuses the import, naming the row
// (goodqa#15087, main WAL wbdc1146b5e3).
func importProvider(row string, p *string) (domain.Provider, error) {
	if p == nil || domain.Provider(*p) == domain.ProviderClaudeCode {
		return domain.ProviderClaudeCode, nil
	}
	return "", fmt.Errorf("%w: import row %s has provider %q; imports create claude-code missions only", domain.ErrInvalidProvider, row, *p)
}

func QueuedItem(raw json.RawMessage) (name, ticket string) {
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
