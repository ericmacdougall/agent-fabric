package reviewgate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"fabric/internal/agent"
)

// goodqa#15864 (C1): the approver sees the change, and an approval holds only
// for the file state it was given for.

func hex64(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

func boundProposal(b *Binding) Proposal {
	p := coreProposal()
	p.Tool = "edit"
	p.Binding = b
	return p
}

func testBinding() *Binding {
	return &Binding{Path: "src/a.go", TargetSHA256: hex64("file"), NewSHA256: hex64("new"),
		DiffSHA256: hex64("diff"), Shown: "--- a/src/a.go\n+++ b/src/a.go\n@@\n-x\n+y\n"}
}

func bindingPolicy() Policy {
	return Policy{Version: PolicyV1, ModelMaxTier: TierLow, Tools: map[string]ToolPolicy{
		"edit": {Allow: true, Risk: TierLow, Reversibility: Reversible, HumanRequired: true, RequireBinding: true},
	}}
}

func TestBindingChangesTheRequestHash(t *testing.T) {
	base, err := RequestHash(boundProposal(testBinding()))
	if err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*Binding){
		"path":        func(b *Binding) { b.Path = "src/b.go" },
		"target sha":  func(b *Binding) { b.TargetSHA256 = hex64("other file state") },
		"target new":  func(b *Binding) { b.TargetSHA256 = "new" },
		"new sha":     func(b *Binding) { b.NewSHA256 = hex64("other new") },
		"diff digest": func(b *Binding) { b.DiffSHA256 = hex64("other diff") },
		"shown text":  func(b *Binding) { b.Shown += "+z\n" },
		"truncated":   func(b *Binding) { b.Truncated = true },
	} {
		b := testBinding()
		mut(b)
		h, err := RequestHash(boundProposal(b))
		if err != nil || h == base {
			t.Errorf("%s: hash unchanged (%v)", name, err)
		}
	}
	if h, _ := RequestHash(boundProposal(nil)); h == base {
		t.Error("dropping the binding keeps the hash")
	}
}

func TestUnboundCanonicalFormIsUnchanged(t *testing.T) {
	c, err := CanonicalProposal(coreProposal())
	if err != nil || bytes.Contains(c, []byte("binding")) {
		t.Fatalf("a proposal without a binding must keep its old canonical form: %s %v", c, err)
	}
	for _, bad := range []*Binding{
		{Path: "", TargetSHA256: "new", NewSHA256: hex64("a"), DiffSHA256: hex64("b")},
		{Path: "p", TargetSHA256: "short", NewSHA256: hex64("a"), DiffSHA256: hex64("b")},
		{Path: "p", TargetSHA256: "new", NewSHA256: "", DiffSHA256: hex64("b")},
		{Path: "p", TargetSHA256: "new", NewSHA256: hex64("a"), DiffSHA256: hex64("b"), Shown: strings.Repeat("x", MaxBindingShownBytes+1)},
		{Path: "p", TargetSHA256: "new", NewSHA256: hex64("a"), DiffSHA256: hex64("b"), Shown: "bad \xff utf8"},
	} {
		if _, err := CanonicalProposal(boundProposal(bad)); !errors.Is(err, ErrInvalid) {
			t.Errorf("invalid binding accepted: %+v (%v)", bad, err)
		}
	}
}

func TestClassifyRequiresBinding(t *testing.T) {
	if _, err := Classify(boundProposal(nil), bindingPolicy()); !errors.Is(err, ErrDenied) {
		t.Fatalf("an unbound edit proposal was classified: %v", err)
	}
	if _, err := Classify(boundProposal(testBinding()), bindingPolicy()); err != nil {
		t.Fatalf("a bound edit proposal was refused: %v", err)
	}
	// RequireBinding is part of the policy digest, so an approval from before it
	// existed does not carry over
	a, _ := PolicyDigest(bindingPolicy())
	p := bindingPolicy()
	e := p.Tools["edit"]
	e.RequireBinding = false
	p.Tools["edit"] = e
	b, _ := PolicyDigest(p)
	if a == b {
		t.Fatal("RequireBinding is not in the policy digest")
	}
}

// ---- executor ----

type fileTool struct {
	path string
	runs atomic.Int32
	last atomic.Value // json.RawMessage
}

func (*fileTool) Name() string         { return "edit" }
func (*fileTool) Def() json.RawMessage { return json.RawMessage(`{"name":"edit"}`) }
func (*fileTool) SideEffecting() bool  { return true }
func (f *fileTool) Run(_ context.Context, in json.RawMessage) (string, bool) {
	f.runs.Add(1)
	f.last.Store(in)
	return "edited", false
}

// preview reads the file the way a real previewer would: path, current sha256,
// and a "diff" that depends on the file bytes.
func (f *fileTool) preview(_ context.Context, args json.RawMessage) (*Binding, error) {
	b, err := os.ReadFile(f.path)
	if err != nil {
		return nil, err
	}
	var in struct {
		Path string `json:"path"`
		New  string `json:"new"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, err
	}
	diff := "-" + string(b) + "\n+" + in.New + "\n"
	sum := sha256.Sum256(b)
	return &Binding{Path: in.Path, TargetSHA256: hex.EncodeToString(sum[:]), NewSHA256: hex64(in.New),
		DiffSHA256: hex64(diff), Shown: diff}, nil
}

func execRequest(t *testing.T, p Proposal) DispatchRequest {
	t.Helper()
	c, err := CanonicalProposal(p)
	if err != nil {
		t.Fatal(err)
	}
	return DispatchRequest{RequestHash: hashBytes(c), CanonicalProposal: c, IdempotencyKey: "k", Fence: 1}
}

func newFileTool(t *testing.T, content string) *fileTool {
	t.Helper()
	f := &fileTool{path: filepath.Join(t.TempDir(), "a.txt")}
	if err := os.WriteFile(f.path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestExecutorRefusesChangedFileOldFormAndContradiction(t *testing.T) {
	ctx := context.Background()
	f := newFileTool(t, "alpha")
	ex := &agentToolExecutor{tool: f, preview: f.preview}
	args := json.RawMessage(`{"path":"a.txt","new":"beta"}`)
	bound := func() Proposal {
		b, err := f.preview(ctx, args)
		if err != nil {
			t.Fatal(err)
		}
		p := coreProposal()
		p.Tool, p.Args, p.Binding = "edit", args, b
		return p
	}

	// unchanged: runs once, and the tool is handed the reviewed file state
	res, err := ex.Execute(ctx, execRequest(t, bound()))
	if err != nil || res.Outcome != Confirmed || f.runs.Load() != 1 {
		t.Fatalf("unchanged file: %+v %v runs=%d", res, err, f.runs.Load())
	}
	var got map[string]string
	_ = json.Unmarshal(f.last.Load().(json.RawMessage), &got)
	if got["expect_sha256"] != hex64("alpha") {
		t.Fatalf("the tool was not told to expect the reviewed sha256: %v", got)
	}

	// the file changes after the proposal was bound: refused, tool not run
	req := execRequest(t, bound())
	if err := os.WriteFile(f.path, []byte("alpha, then changed by someone"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = ex.Execute(ctx, req)
	if err != nil || res.Outcome != FailedDefinite || f.runs.Load() != 1 || res.Result == nil || !strings.Contains(res.Result.Output, "refused") {
		t.Fatalf("a changed file ran: %+v %v runs=%d", res, err, f.runs.Load())
	}

	// a request in the old, unbound form is never run through this path
	old := coreProposal()
	old.Tool, old.Args = "edit", args
	res, err = ex.Execute(ctx, execRequest(t, old))
	if err != nil || res.Outcome != FailedDefinite || f.runs.Load() != 1 || !strings.Contains(res.Result.Output, "predates binding") {
		t.Fatalf("an unbound request ran: %+v %v runs=%d", res, err, f.runs.Load())
	}

	// the call's own expect_sha256 must not contradict the reviewed state
	if err := os.WriteFile(f.path, []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	cargs := json.RawMessage(`{"path":"a.txt","new":"beta","expect_sha256":"` + hex64("something else") + `"}`)
	b, _ := f.preview(ctx, cargs)
	p := coreProposal()
	p.Tool, p.Args, p.Binding = "edit", cargs, b
	res, err = ex.Execute(ctx, execRequest(t, p))
	if err != nil || res.Outcome != FailedDefinite || f.runs.Load() != 1 {
		t.Fatalf("a contradicting expect_sha256 ran: %+v %v runs=%d", res, err, f.runs.Load())
	}
}

func TestNewGatedToolRequiresPreviewForBoundTool(t *testing.T) {
	st, _ := openTestStore(t)
	f := newFileTool(t, "x")
	_, err := NewGatedTool(f, st, Config{Policy: bindingPolicy(), Mode: Enforce, StateReader: staticStateReader{}},
		AgentToolConfig{MissionID: "m_1", SessionID: "s_1", ProposerID: "agent:s_1", OriginalTask: "task"})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("a bound tool was wrapped without a preview: %v", err)
	}
}

// ---- operator view and the approval round trip (PostgreSQL) ----

func gatedFileTool(t *testing.T, st *PostgresStore, f *fileTool, policy Policy) *GatedTool {
	t.Helper()
	g, err := NewGatedTool(f, st, Config{Policy: policy, Mode: Enforce, StateReader: staticStateReader{}},
		AgentToolConfig{MissionID: "m_1", SessionID: fmt.Sprintf("s_%d", proposalSequence.Add(1)), ProposerID: "agent:s_1",
			OriginalTask: "edit the file", Preview: f.preview})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func callCtx(id string) context.Context {
	return agent.WithToolCallContext(context.Background(), agent.ToolCallContext{ID: id, MessageIndex: 1})
}

func pendingEditHash(t *testing.T, ctx context.Context, st *PostgresStore) string {
	t.Helper()
	var h string
	if err := st.pool.QueryRow(ctx, `SELECT request_hash FROM review_gate_requests WHERE state = 'AWAITING_HUMAN' AND tool = 'edit'
		ORDER BY created_at DESC LIMIT 1`).Scan(&h); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestOperatorViewShowsPathShaAndChange(t *testing.T) {
	st, ctx := openTestStore(t)
	f := newFileTool(t, "alpha")
	gt := gatedFileTool(t, st, f, bindingPolicy())
	in := json.RawMessage(`{"path":"a.txt","new":"beta","note":"top-secret-payload"}`)
	res, err := gt.RunReviewed(callCtx("toolu_view01"), in)
	if err != nil || res.Pause == nil || !strings.Contains(res.Output, "approval_required") {
		t.Fatalf("expected a pause: %+v %v", res, err)
	}
	op, _ := NewOperator(st)
	s, err := op.Inspect(ctx, res.Pause.RequestHash)
	if err != nil {
		t.Fatal(err)
	}
	if s.Target == nil || s.Unbound || s.Target.Path != "a.txt" || s.Target.TargetSHA256 != hex64("alpha") ||
		!strings.Contains(s.Target.Shown, "-alpha") || !strings.Contains(s.Target.Shown, "+beta") || s.Target.DiffSHA256 == "" {
		t.Fatalf("the view lacks the path, the file's sha256 or the change: %+v", s.Target)
	}
	// the redaction promise: nothing else of the arguments is shown
	enc, _ := json.Marshal(s)
	if strings.Contains(string(enc), "top-secret-payload") || strings.Contains(string(enc), "canonical_proposal") {
		t.Fatalf("the view exposed other arguments: %s", enc)
	}
	listed, err := op.List(ctx, StateAwaitingHuman, 10)
	if err != nil || len(listed) != 1 || listed[0].Target == nil || listed[0].Target.Shown != "" || listed[0].Target.Path != "a.txt" {
		t.Fatalf("the list shows metadata and the path, not the change: %+v %v", listed, err)
	}
}

func TestApprovalIsRefusedAfterTheFileChanges(t *testing.T) {
	st, ctx := openTestStore(t)
	f := newFileTool(t, "alpha")
	gt := gatedFileTool(t, st, f, bindingPolicy())
	op, _ := NewOperator(st)
	in := json.RawMessage(`{"path":"a.txt","new":"beta"}`)
	call := callCtx("toolu_chg01")
	res, err := gt.RunReviewed(call, in)
	if err != nil || res.Pause == nil {
		t.Fatalf("expected a pause: %+v %v", res, err)
	}
	first := res.Pause.RequestHash
	if _, err := op.Approve(ctx, Approval{RequestHash: first, OperatorID: "op:alice", Reason: "reviewed", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	// the file changes after the approval was granted
	if err := os.WriteFile(f.path, []byte("alpha plus an unreviewed line"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = gt.RunReviewed(call, in)
	if err != nil || f.runs.Load() != 0 || res.Pause == nil || res.Pause.RequestHash == first {
		t.Fatalf("an approval for the old file state ran or was reused: %+v %v runs=%d", res, err, f.runs.Load())
	}
	second := res.Pause.RequestHash
	// the old approval stays attached to the old request only
	if d, _ := NewGate0(st).Inspect(ctx, second); d.State != StateAwaitingHuman {
		t.Fatalf("the new request is %s, want AWAITING_HUMAN", d.State)
	}
	// approving the new state runs it, once
	if _, err := op.Approve(ctx, Approval{RequestHash: second, OperatorID: "op:alice", Reason: "reviewed the new state", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	res, err = gt.RunReviewed(call, in)
	if err != nil || res.Pause != nil || f.runs.Load() != 1 {
		t.Fatalf("the approved current state did not run once: %+v %v runs=%d", res, err, f.runs.Load())
	}
}

// NewGate0 is an Inspect-only gate over st.
func NewGate0(st *PostgresStore) *Gate { return &Gate{store: st, threshold: 3} }

func TestOldFormRequestNeverRunsThroughTheBoundPath(t *testing.T) {
	st, ctx := openTestStore(t)
	f := newFileTool(t, "alpha")
	op, _ := NewOperator(st)
	// a request made before this change: policy without RequireBinding, no binding
	oldPolicy := bindingPolicy()
	e := oldPolicy.Tools["edit"]
	e.RequireBinding = false
	oldPolicy.Tools["edit"] = e
	oldGate, err := NewGate(st, LocalPolicyReviewer{Policy: oldPolicy}, executorFunc(confirmedExecute),
		Config{Policy: oldPolicy, Mode: Enforce, StateReader: staticStateReader{}})
	if err != nil {
		t.Fatal(err)
	}
	in := json.RawMessage(`{"path":"a.txt","new":"beta"}`)
	p := coreProposal()
	p.MissionID, p.SessionID, p.Tool, p.Args = "m_1", "s_old", "edit", in
	p.Provenance = []ProvenanceHandle{{ID: "prompt-1", Origin: OriginUser}, {ID: "call-1-toolu_old01", Origin: OriginAgent, DerivedFrom: []string{"prompt-1"}}}
	oldHash := awaitingRequest(t, ctx, oldGate, p)
	if _, err := op.Approve(ctx, Approval{RequestHash: oldHash, OperatorID: "op:alice", Reason: "approved before bindings", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	s, err := op.Inspect(ctx, oldHash)
	if err != nil || !s.Unbound || s.Target != nil {
		t.Fatalf("the old request is not flagged unbound: %+v %v", s, err)
	}
	// the same call arrives on the new path: it makes a new, bound request and
	// the old approval does not run it
	gt, err := NewGatedTool(f, st, Config{Policy: bindingPolicy(), Mode: Enforce, StateReader: staticStateReader{}},
		AgentToolConfig{MissionID: "m_1", SessionID: "s_old", ProposerID: "agent:s_old", OriginalTask: "edit the file", Preview: f.preview})
	if err != nil {
		t.Fatal(err)
	}
	res, err := gt.RunReviewed(agent.WithToolCallContext(context.Background(), agent.ToolCallContext{ID: "toolu_old01", MessageIndex: 1}), in)
	if err != nil || f.runs.Load() != 0 || res.Pause == nil || res.Pause.RequestHash == oldHash {
		t.Fatalf("the old approval ran through the new path: %+v %v runs=%d", res, err, f.runs.Load())
	}
	// and the gate itself refuses to dispatch the old request under the new policy
	newGate, _ := NewGate(st, LocalPolicyReviewer{Policy: bindingPolicy()}, executorFunc(confirmedExecute),
		Config{Policy: bindingPolicy(), Mode: Enforce, StateReader: staticStateReader{}})
	if _, err := newGate.Dispatch(ctx, oldHash, p, "k-old"); err == nil {
		t.Fatal("the old-form request was dispatched under the bound policy")
	}
}
