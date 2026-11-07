package store

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	"fabric/internal/domain"
)

// MissionSpec is what `fab mission new` and the queue import provide.
type MissionSpec struct {
	ID        domain.MissionID
	Name      string
	Ticket    string
	Repo      string
	Branch    string
	Priority  int
	HostNeeds string
	Source    string
	// Provider is required (goodqa#15087): CreateMission takes it explicitly,
	// with no default.
	Provider domain.Provider
}

// sourceGrammar is the provenance rule (R12): user:, ticket:, wal:, file:,
// inferred:. An <agent-message> quoted as user: is caught by the shape, not by
// trust: a user source must name a session and message id.
var sourceGrammar = regexp.MustCompile(`^(user:[A-Za-z0-9-]+/[A-Za-z0-9#:.T-]+ ".+"|user:[A-Za-z0-9-]+ ".+"|queue:[A-Za-z0-9-]+/[^ ]+ ".+"|ticket:GoodQA [a-z0-9-]+#[0-9]+|wal:[^#]+#w[0-9a-f]{11}|file:[^ ]+|inferred: .+)$`)

// ValidSource reports whether s follows the provenance grammar.
func ValidSource(s string) bool { return sourceGrammar.MatchString(s) }

// CreateMissionSpec registers a mission with its queue metadata. Idempotent on
// the id; an existing row is left untouched (the first import wins).
func (s *Store) CreateMissionSpec(ctx context.Context, m MissionSpec) error {
	_, err := s.EnsureMissionSpec(ctx, m)
	return err
}

// Ensured says what an idempotent registration found (review F5): a new
// row, an identical row, or a row whose stored text differs and was kept.
type Ensured int

const (
	Created Ensured = iota
	Unchanged
	Differs
)

// EnsureMissionSpec registers m once and reports whether an existing row
// matches it. A differing row is never overwritten. A provider difference
// is refused outright (goodqa#15087, main WAL wbdc1146b5e3); every other
// difference keeps the first import, as before.
func (s *Store) EnsureMissionSpec(ctx context.Context, m MissionSpec) (Ensured, error) {
	if _, err := domain.ParseMissionID(string(m.ID)); err != nil {
		return Created, err
	}
	if _, err := domain.ParseProvider(string(m.Provider)); err != nil {
		return Created, err
	}
	if m.Ticket == "" {
		return Created, fmt.Errorf("%w: a mission must name a ticket (R5)", domain.ErrInvalidTransition)
	}
	if m.Source != "" && !ValidSource(m.Source) {
		return Created, fmt.Errorf("%w: mission source %q does not follow the provenance grammar (R12)", domain.ErrInvalidTransition, m.Source)
	}
	state, err := json.Marshal(domain.NewMission(m.ID))
	if err != nil {
		return Created, err
	}
	var created bool
	err = s.pool.QueryRow(ctx, `WITH ins AS (
			INSERT INTO missions (id, ticket, repo, branch, state, name, priority, host_needs, source, provider)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) ON CONFLICT (id) DO NOTHING RETURNING id)
		SELECT EXISTS (SELECT 1 FROM ins)`,
		string(m.ID), m.Ticket, m.Repo, m.Branch, state, m.Name, m.Priority, m.HostNeeds, m.Source, string(m.Provider)).Scan(&created)
	if err != nil || created {
		return Created, err
	}
	if err := s.checkProvider(ctx, m.ID, m.Provider); err != nil {
		return Created, err
	}
	var same bool
	err = s.pool.QueryRow(ctx, `SELECT ticket IS NOT DISTINCT FROM $2 AND repo IS NOT DISTINCT FROM $3
		AND branch IS NOT DISTINCT FROM $4 AND name IS NOT DISTINCT FROM $5 AND priority IS NOT DISTINCT FROM $6
		AND host_needs IS NOT DISTINCT FROM $7 AND source IS NOT DISTINCT FROM $8 FROM missions WHERE id = $1`,
		string(m.ID), m.Ticket, m.Repo, m.Branch, m.Name, m.Priority, m.HostNeeds, m.Source).Scan(&same)
	if err != nil {
		return Created, err
	}
	if same {
		return Unchanged, nil
	}
	return Differs, nil
}

// MissionRow is the portfolio view of one mission.
type MissionRow struct {
	ID        domain.MissionID
	Name      string
	Ticket    string
	Priority  int
	HostNeeds string
	Execution domain.ExecutionState
	Candidate domain.CandidateState
	Delivery  domain.DeliveryState
	Terminal  domain.TerminalState
	Version   domain.Version
	Seq       int64
}

// ListMissions returns every mission, slot holders first, then by priority.
func (s *Store) ListMissions(ctx context.Context) ([]MissionRow, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, name, ticket, priority, host_needs, execution, candidate, delivery, terminal,
		intent_revision, attempt_fence, candidate_generation, seq FROM missions
		ORDER BY (execution IN ('ADMITTED','RUNNING','WAITING','PAUSED')) DESC, priority, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MissionRow
	for rows.Next() {
		var r MissionRow
		if err := rows.Scan(&r.ID, &r.Name, &r.Ticket, &r.Priority, &r.HostNeeds, &r.Execution, &r.Candidate, &r.Delivery, &r.Terminal,
			&r.Version.IntentRevision, &r.Version.AttemptFence, &r.Version.CandidateGeneration, &r.Seq); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Ask is one item of the inbox (DESIGN.md 3.11). No expiry, by design (R13).
type Ask struct {
	ID          string
	Mission     domain.MissionID
	Kind        string // decision | permission | credential-link
	Question    string
	Options     []string
	Recommended string
	Source      string
	State       string
	Answer      string
	AnsweredBy  string
	AnswerSrc   string // provenance of the answer (R12)
	Withdrawn   string // withdrawal reason
}

// CreateAsk refuses an ask without a grammatical source (AC-10). Idempotent on id.
func (s *Store) CreateAsk(ctx context.Context, a Ask) error {
	_, err := s.EnsureAsk(ctx, a)
	return err
}

// EnsureAsk registers a once and reports whether an existing ask matches it.
// A differing ask is never overwritten.
func (s *Store) EnsureAsk(ctx context.Context, a Ask) (Ensured, error) {
	if !askID.MatchString(a.ID) {
		return Created, fmt.Errorf("%w: ask id", domain.ErrInvalidID)
	}
	if !ValidSource(a.Source) {
		return Created, fmt.Errorf("%w: ask %s has no grammatical source (R12): %q", domain.ErrInvalidTransition, a.ID, a.Source)
	}
	switch a.Kind {
	case "decision", "permission", "credential-link":
	default:
		return Created, fmt.Errorf("%w: ask kind %q (a chore kind does not exist, R8)", domain.ErrInvalidTransition, a.Kind)
	}
	opts, err := json.Marshal(a.Options)
	if err != nil {
		return Created, err
	}
	if a.Options == nil {
		opts = []byte("[]")
	}
	var mission *string
	if a.Mission != "" {
		m := string(a.Mission)
		mission = &m
	}
	var created bool
	err = s.pool.QueryRow(ctx, `WITH ins AS (
			INSERT INTO asks (id, mission_id, kind, question, options, recommended, source)
			VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT (id) DO NOTHING RETURNING id)
		SELECT EXISTS (SELECT 1 FROM ins)`,
		a.ID, mission, a.Kind, a.Question, opts, a.Recommended, a.Source).Scan(&created)
	if err != nil || created {
		return Created, err
	}
	var same bool
	err = s.pool.QueryRow(ctx, `SELECT mission_id IS NOT DISTINCT FROM $2 AND kind = $3 AND question = $4
		AND options = $5::jsonb AND recommended = $6 AND source = $7 FROM asks WHERE id = $1`,
		a.ID, mission, a.Kind, a.Question, opts, a.Recommended, a.Source).Scan(&same)
	if err != nil {
		return Created, err
	}
	if same {
		return Unchanged, nil
	}
	return Differs, nil
}

// ListAsks returns asks in one state (” for all), oldest first (the inbox order).
func (s *Store) ListAsks(ctx context.Context, state string) ([]Ask, error) {
	return s.listAsks(ctx, state, "")
}

func (s *Store) listAsks(ctx context.Context, state, id string) ([]Ask, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, COALESCE(mission_id, ''), kind, question, options, recommended, source, state,
		COALESCE(answer, ''), COALESCE(answered_by, ''), COALESCE(answer_source, ''), COALESCE(withdrawn_reason, '')
		FROM asks WHERE ($1 = '' OR state = $1) AND ($2 = '' OR id = $2) ORDER BY created_at, id`, state, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Ask
	for rows.Next() {
		var a Ask
		var opts []byte
		if err := rows.Scan(&a.ID, &a.Mission, &a.Kind, &a.Question, &opts, &a.Recommended, &a.Source, &a.State,
			&a.Answer, &a.AnsweredBy, &a.AnswerSrc, &a.Withdrawn); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(opts, &a.Options); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
