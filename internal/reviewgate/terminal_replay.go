package reviewgate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/jackc/pgx/v5"
)

// terminalForCall finds a durable control outcome by the loop-attached handle,
// independently of the state fingerprint embedded in newer request hashes.
// A completed checkpointed call can therefore never become a fresh side effect
// merely because the world changed after its terminal commit.
func (g *Gate) terminalForCall(ctx context.Context, p Proposal) (Decision, bool, error) {
	callID := ""
	for _, handle := range p.Provenance {
		if handle.Origin == OriginAgent {
			callID = handle.ID
			break
		}
	}
	if callID == "" {
		return Decision{}, false, nil
	}
	r, err := scanStored(g.store.pool.QueryRow(ctx, `SELECT `+storedColumns+` FROM review_gate_requests r
		WHERE r.mission_id = $1 AND r.session_id = $2 AND r.tool = $3
		  AND r.canonical_proposal::jsonb->'args' = $4::jsonb
		  AND r.canonical_proposal::jsonb->'provenance' @> jsonb_build_array(jsonb_build_object('id', $5::text))
		  AND r.state IN ('DENIED', 'UNKNOWN', 'CONFIRMED', 'FAILED_DEFINITE')
		ORDER BY r.created_at DESC, r.request_hash DESC LIMIT 1`,
		p.MissionID, p.SessionID, p.Tool, string(p.Args), callID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Decision{}, false, nil
	}
	if err != nil {
		return Decision{}, false, err
	}
	stored, _, err := decodeStoredProposal(r)
	if err != nil {
		return Decision{}, false, err
	}
	var storedArgs, inputArgs any
	if err := json.Unmarshal(stored.Args, &storedArgs); err != nil {
		return Decision{}, false, err
	}
	if err := json.Unmarshal(p.Args, &inputArgs); err != nil {
		return Decision{}, false, err
	}
	if stored.MissionID != p.MissionID || stored.SessionID != p.SessionID || stored.Tool != p.Tool || !reflect.DeepEqual(storedArgs, inputArgs) {
		return Decision{}, false, fmt.Errorf("%w: terminal tool-call identity differs", ErrConflict)
	}
	return decisionFor(r), true, nil
}
