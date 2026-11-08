package effects

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// goodqa#15165 (Kimi K9): a reply GoodQA did not shape as a post or a full
// comment page is ambiguous. Dispatch must not CONFIRM a post with no
// comment id, and Reconcile must not read a malformed or truncated page as
// "no comment carries the trailer" (NOT_APPLIED), which would post again.
func ambiguousServer(t *testing.T, payload *string) *GoodQA {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1,
			"result": map[string]any{"content": []map[string]any{{"type": "text", "text": *payload}}}})
		io.WriteString(w, string(out)) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)
	keyFile := filepath.Join(t.TempDir(), "member.key")
	if err := os.WriteFile(keyFile, []byte("test-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &GoodQA{Endpoint: srv.URL, KeyFile: keyFile}
}

func TestGoodQADispatchWithoutCommentIDIsUnknown(t *testing.T) {
	var payload string
	g := ambiguousServer(t, &payload)
	req := json.RawMessage(`{"issue_id":"i1","body":"b"}`)
	for _, p := range []string{`{}`, `{"comment":{}}`, `{"comment":{"id":""}}`, `{"comment":{"id":7}}`, `{"ok":true}`,
		`{"comment":{"id":"   "}}`, `{"data":{"id":"\t"}}`, `{"id":" "}`} {
		payload = p
		rec, err := g.Dispatch(context.Background(), Submission{Effect: Effect{Request: req}, RemoteKey: "k1"})
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if rec.Outcome != OutcomeUnknown {
			t.Errorf("reply %s: outcome %v, want UNKNOWN (no comment id proves nothing)", p, rec.Outcome)
		}
	}
	payload = `{"comment":{"id":"c_1"}}`
	if rec, err := g.Dispatch(context.Background(), Submission{Effect: Effect{Request: req}, RemoteKey: "k1"}); err != nil || rec.Outcome != OutcomeConfirmed {
		t.Fatalf("well-formed reply: %v %v, want CONFIRMED", rec.Outcome, err)
	}
}

func TestGoodQAReconcileMalformedPageIsUnknown(t *testing.T) {
	var payload string
	g := ambiguousServer(t, &payload)
	e := Effect{Request: json.RawMessage(`{"issue_id":"i1","body":"b"}`), IdemKey: "k1"}
	for _, p := range []string{
		`{}`,
		`{"unexpected":true}`,
		`{"comments":null,"hasMore":false}`,
		`{"comments":{"0":{}},"hasMore":false}`,
		`{"comments":["text"],"hasMore":false}`,
		`{"comments":[{"id":"c1","body":7}],"hasMore":false}`,
		`{"comments":[],"hasMore":"false"}`,
		`{"comments":[],"hasMore":true}`,
		`{"comments":[],"hasMore":true,"nextCursor":""}`,
		// goodqa#15235 g3: a page without hasMore cannot prove the listing ended.
		`{"comments":[]}`,
	} {
		payload = p
		obs, note, err := g.Reconcile(context.Background(), e)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if obs != ObsUnknown {
			t.Errorf("page %s: %v (%s), want UNKNOWN", p, obs, note)
		}
	}
	// Well-formed pages still decide.
	for p, want := range map[string]Observation{
		`{"comments":[],"hasMore":false}`:                                              ObsNotApplied,
		`{"comments":[{"id":"c1","body":"x\n\n` + Trailer + `k1\n"}],"hasMore":false}`: ObsCommitted,
	} {
		payload = p
		if obs, note, err := g.Reconcile(context.Background(), e); err != nil || obs != want {
			t.Errorf("page %s: %v (%s) %v, want %v", p, obs, note, err, want)
		}
	}
}
