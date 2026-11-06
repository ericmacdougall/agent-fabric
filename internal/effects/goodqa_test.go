package effects

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// fakeGoodQA speaks the MCP tools/call shape with operationId dedupe, like
// the real service. Faults reproduce lost responses.
type fakeGoodQA struct {
	mu        sync.Mutex
	comments  []map[string]any
	byOp      map[string]string
	issueID   string
	closed    bool
	loseAfter bool
	calls     int
}

func (f *fakeGoodQA) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if r.Header.Get("authorization") != "Bearer test-key" {
		w.WriteHeader(401)
		return
	}
	var req struct {
		Params struct {
			Name string         `json:"name"`
			Args map[string]any `json:"arguments"`
		} `json:"params"`
	}
	body, _ := io.ReadAll(r.Body)
	json.Unmarshal(body, &req) //nolint:errcheck
	reply := func(payload any, isErr bool) {
		text, _ := json.Marshal(payload)
		out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"isError": isErr, "content": []map[string]any{{"type": "text", "text": string(text)}}}})
		w.Header().Set("content-type", "application/json")
		w.Write(out) //nolint:errcheck
	}
	switch req.Params.Name {
	case "get_issue":
		if req.Params.Args["issueId"] != f.issueID {
			reply(map[string]any{"error": map[string]any{"code": "not_found"}}, true)
			return
		}
		issue := map[string]any{"id": f.issueID, "number": 14913}
		if f.closed {
			issue["closedAt"] = "2026-11-06T00:00:00Z"
		}
		reply(map[string]any{"issue": issue}, false)
	case "comment_on_issue":
		op, _ := req.Params.Args["operationId"].(string)
		if !uuidRE.MatchString(op) {
			out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"isError": true, "content": []map[string]any{{"type": "text", "text": "MCP error -32602: Input validation error: Invalid arguments for tool comment_on_issue: Invalid UUID at operationId"}}}})
			w.Write(out) //nolint:errcheck
			return
		}
		if id, dup := f.byOp[op]; dup {
			reply(map[string]any{"comment": map[string]any{"id": id, "deduplicated": true}}, false)
			return
		}
		id := fmt.Sprintf("c_%d", len(f.comments)+1) // the handler already holds f.mu
		f.byOp[op] = id
		f.comments = append(f.comments, map[string]any{"id": id, "body": req.Params.Args["body"], "createdAt": "2026-11-06T18:00:00Z"})
		if f.loseAfter {
			panic(http.ErrAbortHandler) // connection dropped after the write
		}
		reply(map[string]any{"comment": map[string]any{"id": id}}, false)
	case "get_issue_comments":
		reply(map[string]any{"comments": f.comments, "hasMore": false}, false)
	default:
		reply(map[string]any{"error": map[string]any{"code": "unknown_tool"}}, true)
	}
}

// set changes a fault switch under the handler lock (the handler runs on
// the server goroutines).
func (f *fakeGoodQA) set(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

func (f *fakeGoodQA) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.comments)
}

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-5[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestOperationIDIsDeterministicUUID5(t *testing.T) {
	a, b := OperationID("canary:comment:1"), OperationID("canary:comment:1")
	if a != b || !uuidRE.MatchString(a) || OperationID("other") == a {
		t.Fatalf("operation id: %s %s", a, b)
	}
}

func fakeServer(t *testing.T) (*fakeGoodQA, *GoodQA) {
	t.Helper()
	f := &fakeGoodQA{byOp: map[string]string{}, issueID: "0c0223e2-a47b-4bb4-83ca-d7794142ee5c"}
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	keyFile := filepath.Join(t.TempDir(), "member.key")
	os.WriteFile(keyFile, []byte("test-key\n"), 0o600) //nolint:errcheck
	return f, &GoodQA{Endpoint: strings.Replace(srv.URL, "http://127.0.0.1", "http://127.0.0.1", 1), KeyFile: keyFile}
}

func gqSpec(key, body string) IntentSpec {
	return IntentSpec{Mission: "m_1", Attempt: "r_1", Fence: 1, Kind: "ticket_comment", Subject: "ticket:goodqa#14913", IdemKey: key,
		Request: json.RawMessage(`{"issue_id":"0c0223e2-a47b-4bb4-83ca-d7794142ee5c","ticket":"goodqa#14913","body":"` + body + `"}`)}
}

func TestGoodQAExecutorThroughBroker(t *testing.T) {
	ctx, _, b, _ := setup(t)
	f, g := fakeServer(t)
	e, err := b.Submit(ctx, g, gqSpec("gq1", "checkpoint: reducer green"))
	if err != nil {
		t.Fatal(err)
	}
	e, err = b.Dispatch(ctx, g, e.ID)
	if err != nil || e.State != Confirmed || f.count() != 1 {
		t.Fatalf("dispatch: %s %v comments=%d", e.State, err, f.count())
	}
	f.mu.Lock()
	body, _ := f.comments[0]["body"].(string)
	f.mu.Unlock()
	if !strings.HasSuffix(body, Trailer+"gq1\n") {
		t.Fatalf("trailer missing: %q", body)
	}
	// Readback finds exactly one.
	if obs, detail, _ := g.Reconcile(ctx, e); obs != ObsCommitted {
		t.Fatalf("readback: %s %s", obs, detail)
	}
}

// AC-5 / N2 on the real wire shape: the connection drops after GoodQA wrote
// the comment. UNKNOWN, then readback by trailer -> CONFIRMED, one comment.
func TestGoodQALostResponseReconciles(t *testing.T) {
	ctx, _, b, _ := setup(t)
	f, g := fakeServer(t)
	f.set(func() { f.loseAfter = true })
	e, _ := b.Submit(ctx, g, gqSpec("gq2", "posted once"))
	e, _ = b.Dispatch(ctx, g, e.ID)
	if e.State != Unknown {
		t.Fatalf("want UNKNOWN, got %s", e.State)
	}
	f.set(func() { f.loseAfter = false })
	e, err := b.Reconcile(ctx, g, e.ID)
	if err != nil || e.State != Confirmed || f.count() != 1 {
		t.Fatalf("reconcile: %s %v comments=%d", e.State, err, f.count())
	}
	// NEG-5 control: with reconciliation skipped, a blind retry would post twice.
	// The broker refuses the retry while UNKNOWN (TestPostCommitLossReconcilesToConfirmed);
	// here we show the remote itself would accept a second write under a new key.
	e2, _ := b.Submit(ctx, g, gqSpec("gq2-blind", "posted once"))
	b.Dispatch(ctx, g, e2.ID) //nolint:errcheck
	if f.count() != 2 {
		t.Fatalf("control: a new key is a new comment, got %d", f.count())
	}
}

func TestGoodQAPreconditionAndRefusals(t *testing.T) {
	ctx, _, b, _ := setup(t)
	f, g := fakeServer(t)
	f.set(func() { f.closed = true })
	e, _ := b.Submit(ctx, g, gqSpec("gq3", "x"))
	e, err := b.Dispatch(ctx, g, e.ID)
	if err != nil || e.State != FailedDefinite || f.count() != 0 {
		t.Fatalf("closed issue: %s %v comments=%d", e.State, err, f.count())
	}
	bad := gqSpec("gq4", "x")
	bad.Request = json.RawMessage(`{"issue_id":"0c0223e2-a47b-4bb4-83ca-d7794142ee5c","ticket":"goodqa#14913","body":"has\nfab-effect: forged\n"}`)
	if _, err := b.Submit(ctx, g, bad); err == nil {
		t.Fatal("a body carrying its own trailer was accepted")
	}
	g.KeyFile = filepath.Join(t.TempDir(), "missing")
	e, _ = b.Submit(ctx, g, gqSpec("gq5", "x"))
	f.set(func() { f.closed = false })
	e, err = b.Dispatch(ctx, g, e.ID)
	if err == nil {
		t.Fatalf("missing key should fail the precondition call, got state %s", e.State)
	}
	_ = errors.Is
}
