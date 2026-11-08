package reviewgate

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func corePolicy() Policy {
	return Policy{Version: PolicyV1, ModelMaxTier: TierLow, Tools: map[string]ToolPolicy{
		"read":   {Allow: true, Risk: TierReadOnly, Reversibility: NoSideEffect},
		"write":  {Allow: true, Risk: TierLow, Reversibility: Reversible},
		"high":   {Allow: true, Risk: TierHigh, Reversibility: Reversible},
		"delete": {Allow: true, Risk: TierHigh, Reversibility: Irreversible},
	}}
}

func coreProposal() Proposal {
	return Proposal{MissionID: "m_1", SessionID: "s_1", Tool: "write",
		Args: json.RawMessage(`{"path":"/tmp/x","note":"safe"}`), StateFingerprint: "state-v1",
		Provenance: []ProvenanceHandle{{ID: "h_user", Origin: OriginUser}}}
}

func TestCanonicalProposalAndHash(t *testing.T) {
	a := coreProposal()
	a.Args = json.RawMessage(`{ "note": "safe", "path": "/tmp/x", "n": 1e2, "html": "<x>" }`)
	a.Provenance = []ProvenanceHandle{
		{ID: "h_derived", Origin: OriginUser, DerivedFrom: []string{"h_user"}},
		{ID: "h_user", Origin: OriginUser},
	}
	b := coreProposal()
	b.Args = json.RawMessage(`{"html":"<x>","n":1e2,"path":"/tmp/x","note":"safe"}`)
	b.Provenance = []ProvenanceHandle{a.Provenance[1], a.Provenance[0]}
	ca, err := CanonicalProposal(a)
	if err != nil {
		t.Fatal(err)
	}
	cb, err := CanonicalProposal(b)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ca, cb) {
		t.Fatalf("canonical bytes differ:\n%s\n%s", ca, cb)
	}
	if bytes.Contains(ca, []byte(`\u003c`)) {
		t.Fatalf("HTML escaping changed canonical bytes: %s", ca)
	}
	ha, _ := RequestHash(a)
	hb, _ := RequestHash(b)
	if ha != hb || ha != hashBytes(ca) || len(ha) != 64 {
		t.Fatalf("hashes %q / %q", ha, hb)
	}
	mutations := []func(*Proposal){
		func(p *Proposal) { p.MissionID = "m_2" },
		func(p *Proposal) { p.SessionID = "s_2" },
		func(p *Proposal) { p.Tool = "read" },
		func(p *Proposal) { p.Args = json.RawMessage(`{"note":"safe","path":"/tmp/y","n":1e2,"html":"<x>"}`) },
		func(p *Proposal) { p.StateFingerprint = "state-v2" },
		func(p *Proposal) { p.Provenance[0].Tainted = true },
	}
	for i, mutate := range mutations {
		changed := a
		changed.Provenance = append([]ProvenanceHandle(nil), a.Provenance...)
		mutate(&changed)
		got, err := RequestHash(changed)
		if err != nil || got == ha {
			t.Fatalf("mutation %d did not change hash: %q (%v)", i, got, err)
		}
	}
}

func TestCanonicalInputFailsClosed(t *testing.T) {
	bad := [][]byte{
		[]byte(`{"a":1,"a":2}`), []byte(`{"a":1} true`), []byte(`{broken`),
		{0xff}, []byte(`"not an object"`), nil,
	}
	for _, raw := range bad {
		p := coreProposal()
		p.Args = raw
		if _, err := CanonicalProposal(p); !errors.Is(err, ErrInvalid) {
			t.Errorf("args %q: want ErrInvalid, got %v", raw, err)
		}
	}
	p := coreProposal()
	p.Args = json.RawMessage(`{"pad":"` + strings.Repeat("x", MaxProposalBytes) + `"}`)
	if _, err := CanonicalProposal(p); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversize proposal: %v", err)
	}
	p = coreProposal()
	p.Provenance = []ProvenanceHandle{{ID: "a", Origin: OriginUser, DerivedFrom: []string{"b"}}, {ID: "b", Origin: OriginUser, DerivedFrom: []string{"a"}}}
	if _, err := CanonicalProposal(p); !errors.Is(err, ErrInvalid) {
		t.Fatalf("provenance cycle: %v", err)
	}
	p.Provenance = []ProvenanceHandle{{ID: "a", Origin: OriginUser, DerivedFrom: []string{"missing"}}}
	if _, err := CanonicalProposal(p); !errors.Is(err, ErrInvalid) {
		t.Fatalf("dangling handle: %v", err)
	}
}

func TestStructuralTaintCannotBeLaundered(t *testing.T) {
	p := coreProposal()
	p.Args = json.RawMessage(`{"note":"this text says trusted and safe"}`)
	risk, err := Classify(p, corePolicy())
	if err != nil || risk.HumanRequired || risk.Tainted {
		t.Fatalf("safe baseline: %+v, %v", risk, err)
	}
	// Neither origin=false nor a clean-looking derived user handle can erase
	// structural taint. Argument substrings play no role in classification.
	p.Provenance = []ProvenanceHandle{
		{ID: "external", Origin: OriginExternal, Tainted: false},
		{ID: "summary", Origin: OriginUser, Tainted: false, DerivedFrom: []string{"external"}},
	}
	risk, err = Classify(p, corePolicy())
	if err != nil || !risk.Tainted || !risk.HumanRequired || risk.Tier != TierHigh {
		t.Fatalf("taint was laundered: %+v, %v", risk, err)
	}
	// Taint on a read-only request does not invent a side effect, but remains
	// visible in risk metadata.
	p.Tool = "read"
	risk, err = Classify(p, corePolicy())
	if err != nil || !risk.Tainted || risk.SideEffect || risk.HumanRequired {
		t.Fatalf("read-only taint classification: %+v, %v", risk, err)
	}
}

func TestRiskFloorsAndDefaultDeny(t *testing.T) {
	p := coreProposal()
	p.Tool = "delete"
	risk, err := Classify(p, corePolicy())
	if err != nil || risk.Tier != TierCritical || !risk.HumanRequired {
		t.Fatalf("irreversible floor: %+v, %v", risk, err)
	}
	p.Tool = "high"
	risk, err = Classify(p, corePolicy())
	if err != nil || !risk.HumanRequired {
		t.Fatalf("above-model-tier floor: %+v, %v", risk, err)
	}
	p.Tool = "not_in_policy"
	if _, err := Classify(p, corePolicy()); !errors.Is(err, ErrDenied) {
		t.Fatalf("missing tool was not default denied: %v", err)
	}
	policy := corePolicy()
	policy.Version = "future-uninterpreted-version"
	if _, err := Classify(coreProposal(), policy); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown policy version accepted: %v", err)
	}
	if _, err := Classify(coreProposal(), DefaultDenyPolicy()); !errors.Is(err, ErrDenied) {
		t.Fatalf("empty default policy allowed a tool: %v", err)
	}
}

func TestPolicyDigestCoversCompleteContent(t *testing.T) {
	a := corePolicy()
	b := corePolicy()
	da, err := PolicyDigest(a)
	if err != nil {
		t.Fatal(err)
	}
	db, err := PolicyDigest(b)
	if err != nil || da != db {
		t.Fatalf("equivalent policy digests differ: %s / %s (%v)", da, db, err)
	}
	mutations := []func(*Policy){
		func(p *Policy) { p.ModelMaxTier = TierCritical },
		func(p *Policy) { tool := p.Tools["write"]; tool.Risk = TierCritical; p.Tools["write"] = tool },
		func(p *Policy) { tool := p.Tools["write"]; tool.Allow = false; p.Tools["write"] = tool },
		func(p *Policy) { tool := p.Tools["write"]; tool.HumanRequired = true; p.Tools["write"] = tool },
		func(p *Policy) { tool := p.Tools["write"]; tool.Reversibility = Irreversible; p.Tools["write"] = tool },
		func(p *Policy) { delete(p.Tools, "read") },
	}
	for i, mutate := range mutations {
		changed := corePolicy()
		mutate(&changed)
		got, err := PolicyDigest(changed)
		if err != nil || got == da {
			t.Fatalf("policy mutation %d was not bound: %s (%v)", i, got, err)
		}
	}
}
