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
	if _, err := domain.ParseMissionID(string(m.ID)); err != nil {
		return err
	}
	if m.Ticket == "" {
		return fmt.Errorf("%w: a mission must name a ticket (R5)", domain.ErrInvalidTransition)
	}
	if m.Source != "" && !ValidSource(m.Source) {
		return fmt.Errorf("%w: mission source %q does not follow the provenance grammar (R12)", domain.ErrInvalidTransition, m.Source)
	}
	state, err := json.Marshal(domain.NewMission(m.ID))
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO missions (id, ticket, repo, branch, state, name, priority, host_needs, source)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) ON CONFLICT (id) DO NOTHING`,
		string(m.ID), m.Ticket, m.Repo, m.Branch, state, m.Name, m.Priority, m.HostNeeds, m.Source)
	return err
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
}

// CreateAsk refuses an ask without a grammatical source (AC-10). Idempotent on id.
func (s *Store) CreateAsk(ctx context.Context, a Ask) error {
	if a.ID == "" || len(a.ID) > 64 {
		return fmt.Errorf("%w: ask id", domain.ErrInvalidID)
	}
	if !ValidSource(a.Source) {
		return fmt.Errorf("%w: ask %s has no grammatical source (R12): %q", domain.ErrInvalidTransition, a.ID, a.Source)
	}
	switch a.Kind {
	case "decision", "permission", "credential-link":
	default:
		return fmt.Errorf("%w: ask kind %q (a chore kind does not exist, R8)", domain.ErrInvalidTransition, a.Kind)
	}
	opts, err := json.Marshal(a.Options)
	if err != nil {
		return err
	}
	if a.Options == nil {
		opts = []byte("[]")
	}
	var mission *string
	if a.Mission != "" {
		m := string(a.Mission)
		mission = &m
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO asks (id, mission_id, kind, question, options, recommended, source)
		VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT (id) DO NOTHING`,
		a.ID, mission, a.Kind, a.Question, opts, a.Recommended, a.Source)
	return err
}

// ListAsks returns open asks oldest first (the inbox order).
func (s *Store) ListAsks(ctx context.Context, state string) ([]Ask, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, COALESCE(mission_id, ''), kind, question, options, recommended, source, state, COALESCE(answer, '')
		FROM asks WHERE ($1 = '' OR state = $1) ORDER BY created_at, id`, state)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Ask
	for rows.Next() {
		var a Ask
		var opts []byte
		if err := rows.Scan(&a.ID, &a.Mission, &a.Kind, &a.Question, &opts, &a.Recommended, &a.Source, &a.State, &a.Answer); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(opts, &a.Options); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
