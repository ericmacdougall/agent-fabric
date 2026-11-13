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
	"fabric/internal/tools"
)

// ReviewGateConfig selects the rollout explicitly in the grant. The zero mode
// is Shadow for migration compatibility: calls are reviewed and recorded but
// never execute. Enforce is fully live. SafeTools is the only way to permit an
// agent-tainted side-effecting tool without a human approving each call. It
// names edit and write only, and a mission that sets it does not start without
// an operator approval recorded in the audit chain for that mission and that
// exact policy (goodqa#15866, #15867).
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
		"read": {Allow: true, Risk: reviewgate.TierReadOnly, Reversibility: reviewgate.NoSideEffect},
		"grep": {Allow: true, Risk: reviewgate.TierReadOnly, Reversibility: reviewgate.NoSideEffect},
		// A shell command can do anything the confined child can, and nothing
		// records how to undo it: it is not reversible, so the policy treats it
		// as unknown-undo and every call needs a human (goodqa#15867).
		"shell": {Allow: true, Risk: reviewgate.TierLow, Reversibility: reviewgate.UnknownUndo, HumanRequired: true},
		// Option A (goodqa#15361, main (agent) on #15361, conditions C1-C5 from
		// the red-team): every edit and write is reviewed per call, and the
		// reviewer sees the change. Each request binds the target path, the
		// target file's sha256 and the diff (RequireBinding), so an approval
		// holds only for the file state it was given for. safe_tools is the
		// per-mission opt-out; it needs a recorded operator approval (C3).
		// Reversible holds because every replaced file keeps a never-evicted
		// pre-image copy unless git holds the bytes in a gitdir the child
		// cannot write, a full store refuses the call, and the store sits out
		// of the child's reach (goodqa#15361 F1/F2, #15865).
		"edit":  {Allow: true, Risk: reviewgate.TierLow, Reversibility: reviewgate.Reversible, HumanRequired: true, RequireBinding: true},
		"write": {Allow: true, Risk: reviewgate.TierLow, Reversibility: reviewgate.Reversible, HumanRequired: true, RequireBinding: true},
	}}
	for _, name := range c.SafeTools {
		// Only the two file tools may skip per-call review. shell and every
		// other tool always need a human (goodqa#15867).
		if name != "edit" && name != "write" {
			return reviewgate.Policy{}, fmt.Errorf("review_gate.safe_tools may name only \"edit\" and \"write\", not %q; every other tool always needs a human", name)
		}
		tool := p.Tools[name]
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
			OriginalTask: g.Prompt, Preview: previewFor(tool),
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

// filePreviewer is implemented by the edit and write tools.
type filePreviewer interface {
	Preview(ctx context.Context, input json.RawMessage) (tools.Preview, error)
}

// previewFor adapts a file tool's Preview to the gate; nil for every other tool.
func previewFor(tool agent.Tool) reviewgate.PreviewFunc {
	if f, ok := tool.(freshTool); ok {
		tool = f.Tool
	}
	fp, ok := tool.(filePreviewer)
	if !ok {
		return nil
	}
	return func(ctx context.Context, args json.RawMessage) (*reviewgate.Binding, error) {
		p, err := fp.Preview(ctx, args)
		if err != nil {
			return nil, err
		}
		return &reviewgate.Binding{Path: p.Path, TargetSHA256: p.TargetSHA256, NewSHA256: p.NewSHA256,
			DiffSHA256: p.DiffSHA256, Shown: p.Shown, Truncated: p.Truncated}, nil
	}
}

// SafeToolsPolicyDigest is the digest of the policy a grant's review_gate
// produces. An operator approval of safe_tools is recorded against it.
func SafeToolsPolicyDigest(c ReviewGateConfig) (string, error) {
	p, err := c.policy()
	if err != nil {
		return "", err
	}
	return reviewgate.PolicyDigest(p)
}

// requireSafeToolsApproval refuses a grant that sets safe_tools unless an
// operator approval for this mission and this exact policy is in the audit
// chain (goodqa#15866). Nothing a model can call writes that record.
func requireSafeToolsApproval(ctx context.Context, rs *reviewgate.PostgresStore, g Grant) error {
	if len(g.ReviewGate.SafeTools) == 0 {
		return nil
	}
	digest, err := SafeToolsPolicyDigest(g.ReviewGate)
	if err != nil {
		return err
	}
	entry, ok, err := rs.SafeToolsApproval(ctx, g.Mission, digest)
	if err != nil {
		return fmt.Errorf("native: safe_tools approval lookup: %w", err)
	}
	if !ok {
		return fmt.Errorf("native: review_gate.safe_tools %v needs an operator approval recorded for mission %s and policy digest %s; "+
			"an operator runs `FABRIC_ACTOR=op:<name> fab review approve-safe-tools --grant <file> --reason <why>`; refusing to start",
			g.ReviewGate.SafeTools, g.Mission, digest)
	}
	_ = entry
	return nil
}
