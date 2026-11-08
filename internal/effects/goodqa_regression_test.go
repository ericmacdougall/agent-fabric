package effects

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// goodqa#15235 slice 3b: ported from the Kimi tree (g1-g5). The endpoint and
// redirect tests there are not taken: main enforces checkEndpoint and follows
// no redirects (goodqa_endpoint_test.go).

func goodQAForEndpoint(t *testing.T, endpoint string) *GoodQA {
	t.Helper()
	keyFile := filepath.Join(t.TempDir(), "member.key")
	if err := os.WriteFile(keyFile, []byte("test-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &GoodQA{Endpoint: endpoint, KeyFile: keyFile}
}

func writeMCPText(t *testing.T, w http.ResponseWriter, text string, isError bool) {
	t.Helper()
	out, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"result": map[string]any{
			"isError": isError,
			"content": []map[string]any{{"type": "text", "text": text}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	w.Header().Set("content-type", "application/json")
	if _, err := w.Write(out); err != nil {
		t.Error(err)
	}
}

func directCommentSubmission(key string) Submission {
	return Submission{
		Effect:    Effect{IdemKey: key, Request: gqSpec(key, "regression").Request},
		RemoteKey: key,
	}
}

// g1: the key travels in a trailer line that readback matches with
// [A-Za-z0-9._:-]{1,64}; a key outside that grammar could never be found
// again, so a post would always reconcile to NOT_APPLIED and repeat.
func TestGoodQAValidateIdempotencyKeyGrammar(t *testing.T) {
	g := &GoodQA{}
	for _, key := range []string{"a", "A0._:-", strings.Repeat("k", 64)} {
		if err := g.Validate(gqSpec(key, "x")); err != nil {
			t.Errorf("valid key %q refused: %v", key, err)
		}
	}
	for _, key := range []string{"", strings.Repeat("k", 65), "has space", "slash/key", "line\nbreak", "tab\tkey", "unicode-é"} {
		if err := g.Validate(gqSpec(key, "x")); err == nil {
			t.Errorf("invalid key %q accepted", key)
		}
	}
}

// g5: a plain-text MCP error does not show whether the write happened.
func TestGoodQAUnstructuredMCPErrorIsUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeMCPText(t, w, "MCP error -32602: Input validation error", true)
	}))
	defer srv.Close()
	g := goodQAForEndpoint(t, srv.URL)
	receipt, err := g.Dispatch(context.Background(), directCommentSubmission("ambiguous"))
	if err != nil {
		t.Fatalf("unstructured MCP error became a transport error: %v", err)
	}
	if receipt.Outcome != OutcomeUnknown {
		t.Fatalf("unstructured MCP error outcome %s, want UNKNOWN", receipt.Outcome)
	}
}

// g5 control: a structured machine code still refuses definitively, so only
// plain text falls through to UNKNOWN.
func TestGoodQAStructuredValidationErrorIsDefinite(t *testing.T) {
	for _, payload := range []string{
		`{"error":{"code":"validation_error","message":"bad operationId"}}`,
		`{"data":{"error":{"code":"validation_error"}}}`,
		`{"error":{"code":"not_found"}}`,
		`{"error":{"code":"forbidden"}}`,
		`{"error":{"code":"closed"}}`,
	} {
		t.Run(payload, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeMCPText(t, w, payload, true)
			}))
			defer srv.Close()
			g := goodQAForEndpoint(t, srv.URL)
			receipt, err := g.Dispatch(context.Background(), directCommentSubmission("structured"))
			if err != nil {
				t.Fatalf("structured refusal became a transport error: %v", err)
			}
			if receipt.Outcome != OutcomeFailedDefinite {
				t.Fatalf("structured refusal outcome %s (%s), want FAILED_DEFINITE", receipt.Outcome, receipt.Detail)
			}
		})
	}
	// An unrecognised structured code is still not definite.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeMCPText(t, w, `{"error":{"code":"internal"}}`, true)
	}))
	defer srv.Close()
	receipt, err := goodQAForEndpoint(t, srv.URL).Dispatch(context.Background(), directCommentSubmission("structured"))
	if err != nil || receipt.Outcome != OutcomeUnknown {
		t.Fatalf("unknown structured code: %s %v, want UNKNOWN", receipt.Outcome, err)
	}
}

// g2: an archived comment is still evidence of the write.
func TestGoodQAArchivedTrailerRemainsCommitted(t *testing.T) {
	f, g := fakeServer(t)
	const key = "archived-key"
	f.set(func() {
		f.comments = []map[string]any{{
			"id":         "c_archived",
			"body":       "posted before archival\n\n" + Trailer + key + "\n",
			"archivedAt": "2026-11-07T00:00:00Z",
		}}
	})
	e := Effect{IdemKey: key, Request: gqSpec(key, "regression").Request}
	obs, detail, err := g.Reconcile(context.Background(), e)
	if err != nil || obs != ObsCommitted {
		t.Fatalf("archived keyed comment: %s %q %v, want COMMITTED", obs, detail, err)
	}
}

func reconcileReadbackPayload(t *testing.T, key, payload string) (Observation, string, error) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeMCPText(t, w, payload, false)
	}))
	t.Cleanup(srv.Close)
	g := goodQAForEndpoint(t, srv.URL)
	e := Effect{IdemKey: key, Request: gqSpec(key, "regression").Request}
	return g.Reconcile(context.Background(), e)
}

// g3/g4: only a well-formed, finished listing can prove absence.
func TestGoodQAMalformedReadbackIsUnknown(t *testing.T) {
	for _, payload := range []string{
		`{}`,
		`null`,
		`{"comments":null,"hasMore":false}`,
		`{"comments":{},"hasMore":false}`,
		`{"comments":"none","hasMore":false}`,
		`{"comments":[]}`,
		`{"comments":[],"hasMore":null}`,
		`{"comments":[],"hasMore":"false"}`,
		`{"comments":[],"hasMore":true}`,
		`{"comments":[],"hasMore":true,"nextCursor":null}`,
		`{"comments":[],"hasMore":true,"nextCursor":""}`,
		`{"comments":[],"hasMore":true,"nextCursor":"   "}`,
		`{"comments":[],"hasMore":true,"nextCursor":123}`,
		`{"comments":[null],"hasMore":false}`,
		`{"comments":["not-an-object"],"hasMore":false}`,
		`{"comments":[123],"hasMore":false}`,
		`{"comments":[{}],"hasMore":false}`,
		`{"comments":[{"body":null}],"hasMore":false}`,
		`{"comments":[{"body":123}],"hasMore":false}`,
	} {
		t.Run(payload, func(t *testing.T) {
			obs, detail, err := reconcileReadbackPayload(t, "malformed-readback", payload)
			if err != nil {
				t.Fatalf("malformed readback returned an error instead of an observation: %v", err)
			}
			if obs != ObsUnknown {
				t.Fatalf("malformed readback: %s %q, want UNKNOWN", obs, detail)
			}
		})
	}
}

func TestGoodQAValidExhaustedEmptyReadbackIsNotApplied(t *testing.T) {
	obs, detail, err := reconcileReadbackPayload(t, "valid-empty", `{"comments":[],"hasMore":false}`)
	if err != nil || obs != ObsNotApplied {
		t.Fatalf("valid exhausted empty listing: %s %q %v, want NOT_APPLIED", obs, detail, err)
	}
}

func TestGoodQAReadbackCursorMustProgress(t *testing.T) {
	obs, detail, err := reconcileReadbackPayload(t, "repeated-cursor", `{"comments":[],"hasMore":true,"nextCursor":"repeat"}`)
	if err != nil || obs != ObsUnknown {
		t.Fatalf("repeated cursor: %s %q %v, want UNKNOWN", obs, detail, err)
	}
	if !strings.Contains(detail, "did not advance") {
		t.Fatalf("repeated cursor detail %q, want progress failure", detail)
	}
}

func TestGoodQAArchivedTrailerAcrossValidPagesRemainsCommitted(t *testing.T) {
	const key = "archived-page-key"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Params struct {
				Args map[string]any `json:"arguments"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		var payload any
		switch cursor, _ := req.Params.Args["cursor"].(string); cursor {
		case "":
			payload = map[string]any{"comments": []any{}, "hasMore": true, "nextCursor": "page-2"}
		case "page-2":
			if req.Params.Args["archiveState"] != "all" {
				payload = map[string]any{"comments": []any{}, "hasMore": false}
				break
			}
			payload = map[string]any{
				"comments": []any{map[string]any{
					"id":         "c_archived_page",
					"body":       "posted before archival\n\n" + Trailer + key + "\n",
					"archivedAt": "2026-11-07T00:00:00Z",
				}},
				"hasMore": false,
			}
		default:
			t.Errorf("unexpected cursor %q", cursor)
			return
		}
		text, err := json.Marshal(payload)
		if err != nil {
			t.Error(err)
			return
		}
		writeMCPText(t, w, string(text), false)
	}))
	defer srv.Close()
	g := goodQAForEndpoint(t, srv.URL)
	e := Effect{IdemKey: key, Request: gqSpec(key, "regression").Request}
	obs, detail, err := g.Reconcile(context.Background(), e)
	if err != nil || obs != ObsCommitted {
		t.Fatalf("archived keyed comment on second page: %s %q %v, want COMMITTED", obs, detail, err)
	}
}
