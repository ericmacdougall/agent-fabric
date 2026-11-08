package reviewgate

import (
	"encoding/json"
	"fmt"
	"sort"
)

// ToolPolicy is an explicit allow-list entry. Missing tools and Allow=false
// are denied. Reversibility and risk come only from this server-side policy.
type ToolPolicy struct {
	Allow         bool
	Risk          RiskTier
	Reversibility Reversibility
	HumanRequired bool
	// AllowTaintedSideEffects is an explicit safe-policy exception. Without it,
	// every side effect derived from an agent or tool handle requires a human.
	AllowTaintedSideEffects bool
}

type Policy struct {
	Version      string
	ModelMaxTier RiskTier
	Tools        map[string]ToolPolicy
}

// DefaultDenyPolicy has no tool capabilities.
func DefaultDenyPolicy() Policy {
	return Policy{Version: PolicyV1, ModelMaxTier: TierReadOnly, Tools: map[string]ToolPolicy{}}
}

func clonePolicy(p Policy) Policy {
	tools := make(map[string]ToolPolicy, len(p.Tools))
	for name, tool := range p.Tools {
		tools[name] = tool
	}
	p.Tools = tools
	return p
}

func validatePolicy(p Policy) error {
	if p.Version != PolicyV1 {
		return invalid("unsupported policy version %q", p.Version)
	}
	if p.ModelMaxTier < TierReadOnly || p.ModelMaxTier > TierCritical {
		return invalid("model tier %d", p.ModelMaxTier)
	}
	for name, tool := range p.Tools {
		if !validText(name, MaxNameBytes) || tool.Risk < TierReadOnly || tool.Risk > TierCritical {
			return invalid("invalid policy for tool %q", name)
		}
		switch tool.Reversibility {
		case NoSideEffect, Reversible, UnknownUndo, Irreversible:
		default:
			return invalid("invalid reversibility for tool %q", name)
		}
	}
	return nil
}

// PolicyDigest authenticates the complete policy content, not merely its
// format version. Map order cannot affect it and every tool rule is included.
func PolicyDigest(p Policy) (string, error) {
	if err := validatePolicy(p); err != nil {
		return "", err
	}
	type wireTool struct {
		Name                    string        `json:"name"`
		Allow                   bool          `json:"allow"`
		Risk                    RiskTier      `json:"risk"`
		Reversibility           Reversibility `json:"reversibility"`
		HumanRequired           bool          `json:"human_required"`
		AllowTaintedSideEffects bool          `json:"allow_tainted_side_effects"`
	}
	type wirePolicy struct {
		Version      string     `json:"version"`
		ModelMaxTier RiskTier   `json:"model_max_tier"`
		Tools        []wireTool `json:"tools"`
	}
	names := make([]string, 0, len(p.Tools))
	for name := range p.Tools {
		names = append(names, name)
	}
	sort.Strings(names)
	w := wirePolicy{Version: p.Version, ModelMaxTier: p.ModelMaxTier, Tools: make([]wireTool, 0, len(names))}
	for _, name := range names {
		t := p.Tools[name]
		w.Tools = append(w.Tools, wireTool{name, t.Allow, t.Risk, t.Reversibility, t.HumanRequired, t.AllowTaintedSideEffects})
	}
	b, err := json.Marshal(w)
	if err != nil {
		return "", fmt.Errorf("%w: policy digest: %v", ErrInvalid, err)
	}
	return hashBytes(b), nil
}

// Classify applies default-deny policy and monotonic risk floors. Taint and
// irreversibility can only escalate; model output is not an input.
func Classify(p Proposal, policy Policy) (RiskAssessment, error) {
	if _, err := CanonicalProposal(p); err != nil {
		return RiskAssessment{}, err
	}
	digest, err := PolicyDigest(policy)
	if err != nil {
		return RiskAssessment{}, err
	}
	tool, ok := policy.Tools[p.Tool]
	if !ok || !tool.Allow {
		return RiskAssessment{}, fmt.Errorf("%w: tool %q is not allowed by %s", ErrDenied, p.Tool, policy.Version)
	}
	r := RiskAssessment{
		Tier: tool.Risk, Reversibility: tool.Reversibility,
		Tainted: proposalTainted(p.Provenance), SideEffect: tool.Reversibility != NoSideEffect,
		HumanRequired: tool.HumanRequired, PolicyVersion: policy.Version, PolicyDigest: digest,
	}
	if tool.Reversibility == UnknownUndo || (r.Tainted && r.SideEffect && !tool.AllowTaintedSideEffects) {
		r.HumanRequired = true
		if r.Tier < TierHigh {
			r.Tier = TierHigh
		}
	}
	if tool.Reversibility == Irreversible {
		r.HumanRequired = true
		r.Tier = TierCritical
	}
	if r.Tier > policy.ModelMaxTier {
		r.HumanRequired = true
	}
	return r, nil
}
