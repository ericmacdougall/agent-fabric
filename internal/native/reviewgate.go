package native

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"fabric/internal/agent"
	"fabric/internal/reviewgate"
	"fabric/internal/store"
)

// ReviewGateConfig selects the rollout explicitly in the grant. The zero mode
// is Shadow for migration compatibility: calls are reviewed and recorded but
// never execute. Enforce is fully live. SafeTools is the only way to permit an
// agent-tainted side-effecting tool without separate human approval.
type ReviewGateConfig struct {
	Mode      string   `json:"mode,omitempty"`
	SafeTools []string `json:"safe_tools,omitempty"`
}

// set reports whether the grant configures the gate at all.
func (c ReviewGateConfig) set() bool { return c.Mode != "" || len(c.SafeTools) > 0 }

func (c ReviewGateConfig) mode() (reviewgate.RolloutMode, error) {
	mode := reviewgate.RolloutMode(c.Mode)
	if mode == "" {
		mode = reviewgate.Shadow
	}
	switch mode {
	case reviewgate.Shadow, reviewgate.Evaluate, reviewgate.Enforce:
		return mode, nil
	default:
		return "", fmt.Errorf("review_gate.mode must be shadow, evaluate or enforce")
	}
}

func (c ReviewGateConfig) policy() (reviewgate.Policy, error) {
	p := reviewgate.Policy{Version: reviewgate.PolicyV1, ModelMaxTier: reviewgate.TierLow, Tools: map[string]reviewgate.ToolPolicy{
		"read":  {Allow: true, Risk: reviewgate.TierReadOnly, Reversibility: reviewgate.NoSideEffect},
		"grep":  {Allow: true, Risk: reviewgate.TierReadOnly, Reversibility: reviewgate.NoSideEffect},
		"shell": {Allow: true, Risk: reviewgate.TierLow, Reversibility: reviewgate.Reversible, HumanRequired: true},
		// Option A (goodqa#15361, decision: main (agent), on #15361): every edit
		// and write is reviewed per call. safe_tools is the per-mission opt-out.
		// Reversible holds because every replaced file git cannot restore
		// (untracked, dirty, staged-only) keeps a never-evicted pre-image copy,
		// and a full store refuses the call (goodqa#15361 r1b F1/F2).
		"edit":  {Allow: true, Risk: reviewgate.TierLow, Reversibility: reviewgate.Reversible, HumanRequired: true},
		"write": {Allow: true, Risk: reviewgate.TierLow, Reversibility: reviewgate.Reversible, HumanRequired: true},
	}}
	for _, name := range c.SafeTools {
		tool, ok := p.Tools[name]
		if !ok {
			return reviewgate.Policy{}, fmt.Errorf("review_gate.safe_tools names unknown tool %q", name)
		}
		tool.HumanRequired = false
		tool.AllowTaintedSideEffects = true
		p.Tools[name] = tool
	}
	return p, nil
}

// AttemptStateReader derives fresh fingerprints only from authoritative native
// attempt/mission rows. Lease timestamps are excluded because heartbeats extend
// them; expiry itself is included, as are fences and lifecycle state changes.
type AttemptStateReader struct {
	Pool *pgxpool.Pool
}

func (r AttemptStateReader) CurrentFingerprint(ctx context.Context, missionID, sessionID string) (string, error) {
	if r.Pool == nil {
		return "", fmt.Errorf("native review state reader has no database")
	}
	var state struct {
		Mission    string `json:"mission"`
		Attempt    string `json:"attempt"`
		Fence      int64  `json:"fence"`
		State      string `json:"state"`
		Path       string `json:"path"`
		Host       string `json:"host"`
		Execution  string `json:"execution"`
		Terminal   string `json:"terminal"`
		Delivery   string `json:"delivery"`
		LeaseFresh bool   `json:"lease_fresh"`
	}
	state.Mission, state.Attempt = missionID, sessionID
	err := r.Pool.QueryRow(ctx, `SELECT a.fence, a.state, a.path, coalesce(a.host, ''),
		m.execution, m.terminal, m.delivery,
		(a.lease_until IS NOT NULL AND a.lease_until > clock_timestamp())
		FROM attempts a JOIN missions m ON m.id = a.mission_id
		WHERE a.id = $1 AND a.mission_id = $2`, sessionID, missionID).
		Scan(&state.Fence, &state.State, &state.Path, &state.Host, &state.Execution,
			&state.Terminal, &state.Delivery, &state.LeaseFresh)
	if err != nil {
		return "", fmt.Errorf("native review state: %w", err)
	}
	canonical, err := json.Marshal(state)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(canonical)
	return "native-" + hex.EncodeToString(digest[:]), nil
}

func wrapReviewTools(toolset []agent.Tool, st *store.Store, reviewStore *reviewgate.PostgresStore, g Grant, redactor reviewgate.Redactor) ([]agent.Tool, error) {
	mode, err := g.ReviewGate.mode()
	if err != nil {
		return nil, err
	}
	policy, err := g.ReviewGate.policy()
	if err != nil {
		return nil, err
	}
	out := make([]agent.Tool, 0, len(toolset))
	for _, tool := range toolset {
		wrapped, err := reviewgate.NewGatedTool(tool, reviewStore, reviewgate.Config{
			Policy: policy, Mode: mode, StateReader: AttemptStateReader{Pool: st.Pool()}, Redactor: redactor,
		}, reviewgate.AgentToolConfig{
			MissionID: g.Mission, SessionID: g.Attempt, ProposerID: "agent:" + g.Attempt,
			OriginalTask: g.Prompt,
		})
		if err != nil {
			return nil, fmt.Errorf("review gate %s: %w", tool.Name(), err)
		}
		out = append(out, wrapped)
	}
	return out, nil
}

func validateReviewGateConfig(c ReviewGateConfig) error {
	if _, err := c.mode(); err != nil {
		return err
	}
	_, err := c.policy()
	return err
}

func reviewGatePin(c ReviewGateConfig) string {
	mode := c.Mode
	if mode == "" {
		mode = string(reviewgate.Shadow)
	}
	b, _ := json.Marshal(struct {
		Mode string
		Safe []string
	}{mode, c.SafeTools})
	return string(b)
}
