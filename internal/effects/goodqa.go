package effects

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// GoodQA posts ticket comments through the GoodQA MCP endpoint (tools/call),
// the same wire shape the installed ticket_adapters.py uses. The business
// idempotency key travels as the tool's operationId AND as a trailer line in
// the body, so a readback can match the comment without any token
// (DESIGN.md 3.10, risk table "reconciliation cannot distinguish its own
// comment"). The key is read from a 0600 file and never logged.
type GoodQA struct {
	Endpoint string
	KeyFile  string
	Client   *http.Client
	MaxPages int
}

const Trailer = "fab-effect: "

// OperationID is the remote idempotency key: GoodQA requires a UUID, so it
// is uuid5(NAMESPACE_URL, "fabric-effect:"+key), deterministic per business
// key (the same derivation shape ah_ticket.py uses). The raw key stays in the
// body trailer for readback.
func OperationID(idemKey string) string {
	ns := []byte{0x6b, 0xa7, 0xb8, 0x11, 0x9d, 0xad, 0x11, 0xd1, 0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8} // NAMESPACE_URL
	h := sha1.New()
	h.Write(ns)
	h.Write([]byte("fabric-effect:" + idemKey))
	u := h.Sum(nil)[:16]
	u[6] = (u[6] & 0x0f) | 0x50
	u[8] = (u[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

var trailerRE = regexp.MustCompile(`(?m)^fab-effect: ([A-Za-z0-9._:-]{1,64})\s*$`)

type commentRequest struct {
	IssueID string `json:"issue_id"`
	Ticket  string `json:"ticket"`
	Body    string `json:"body"`
}

func (g *GoodQA) Kind() string { return "ticket_comment" }

func (g *GoodQA) Validate(spec IntentSpec) error {
	var r commentRequest
	if err := json.Unmarshal(spec.Request, &r); err != nil {
		return err
	}
	if len(r.IssueID) < 8 || r.Ticket == "" || strings.TrimSpace(r.Body) == "" {
		return errors.New("ticket_comment needs issue_id, ticket and body")
	}
	if len(r.Body) > 60_000 {
		return errors.New("ticket_comment body exceeds 60000 bytes")
	}
	if strings.Contains(r.Body, Trailer) {
		return errors.New("ticket_comment body must not contain the trailer; the executor appends it")
	}
	return nil
}

// Precondition: the issue is readable and not closed.
func (g *GoodQA) Precondition(ctx context.Context, e Effect) (bool, string, error) {
	var r commentRequest
	if err := json.Unmarshal(e.Request, &r); err != nil {
		return false, "", err
	}
	data, err := g.call(ctx, "get_issue", map[string]any{"issueId": r.IssueID})
	if err != nil {
		return false, "", err
	}
	if code, ok := data["__error__"]; ok {
		return false, fmt.Sprintf("issue unreadable: %v", code), nil
	}
	issue := data
	if inner, ok := data["issue"].(map[string]any); ok {
		issue = inner
	}
	if id, _ := issue["id"].(string); id != r.IssueID {
		return false, "issue id mismatch", nil
	}
	if closed, _ := issue["closedAt"].(string); closed != "" {
		return false, "issue is closed", nil
	}
	return true, "", nil
}

// Dispatch posts the comment with the key as operationId and trailer.
func (g *GoodQA) Dispatch(ctx context.Context, sub Submission) (Receipt, error) {
	var r commentRequest
	if err := json.Unmarshal(sub.Effect.Request, &r); err != nil {
		return Receipt{}, err
	}
	body := strings.TrimRight(r.Body, "\n") + "\n\n" + Trailer + sub.RemoteKey + "\n"
	data, err := g.call(ctx, "comment_on_issue", map[string]any{"issueId": r.IssueID, "body": body, "operationId": OperationID(sub.RemoteKey)})
	if err != nil {
		return Receipt{}, err // transport: the broker records UNKNOWN
	}
	if code, ok := data["__error__"]; ok {
		c := fmt.Sprint(code)
		switch c {
		case "not_found", "forbidden", "validation_error", "closed":
			return Receipt{Outcome: OutcomeFailedDefinite, Detail: "goodqa refused: " + c}, nil
		}
		return Receipt{Outcome: OutcomeUnknown, Detail: "goodqa error: " + c}, nil
	}
	// goodqa#15165: a success with no comment id proves nothing; reconcile
	// reads the issue back and decides.
	id := commentID(data)
	if id == "" {
		return Receipt{Outcome: OutcomeUnknown, Detail: "goodqa reply carried no comment id"}, nil
	}
	remote, _ := json.Marshal(map[string]any{"comment_id": id})
	return Receipt{Outcome: OutcomeConfirmed, Detail: "comment posted", Remote: remote}, nil
}

func firstN(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func commentID(data map[string]any) string {
	for _, k := range []string{"comment", "data"} {
		if inner, ok := data[k].(map[string]any); ok {
			if id, _ := inner["id"].(string); id != "" {
				return id
			}
		}
	}
	id, _ := data["id"].(string)
	return id
}

// Reconcile reads the issue's comments and looks for the trailer with this
// effect's key: exactly one is COMMITTED, none is NOT_APPLIED, more than one
// is CONFLICT (quarantine: something posted twice).
func (g *GoodQA) Reconcile(ctx context.Context, e Effect) (Observation, string, error) {
	var r commentRequest
	if err := json.Unmarshal(e.Request, &r); err != nil {
		return ObsUnknown, "", err
	}
	pages := g.MaxPages
	if pages <= 0 {
		pages = 5
	}
	cursor := ""
	matches := 0
	for page := 0; page < pages; page++ {
		args := map[string]any{"issueId": r.IssueID, "order": "asc", "limit": 200}
		if cursor != "" {
			args["cursor"] = cursor
		}
		data, err := g.call(ctx, "get_issue_comments", args)
		if err != nil {
			return ObsUnknown, "readback transport: " + err.Error(), nil
		}
		if code, ok := data["__error__"]; ok {
			return ObsUnknown, fmt.Sprintf("readback refused: %v", code), nil
		}
		// goodqa#15165: only a well-formed page can prove absence. A missing
		// or malformed list, item or paging field is UNKNOWN, never NOT_APPLIED.
		list, ok := data["comments"].([]any)
		if !ok {
			return ObsUnknown, "readback page has no comments list", nil
		}
		for _, item := range list {
			c, ok := item.(map[string]any)
			if !ok {
				return ObsUnknown, "readback page has a malformed comment", nil
			}
			if c["archivedAt"] != nil && c["archivedAt"] != "" {
				continue
			}
			body, ok := c["body"].(string)
			if !ok {
				return ObsUnknown, "readback page has a comment without a body", nil
			}
			for _, m := range trailerRE.FindAllStringSubmatch(body, -1) {
				if m[1] == e.IdemKey {
					matches++
				}
			}
		}
		more := false
		if v, present := data["hasMore"]; present {
			if more, ok = v.(bool); !ok {
				return ObsUnknown, "readback page has a malformed hasMore", nil
			}
		}
		cursor, _ = data["nextCursor"].(string)
		if more && cursor == "" {
			return ObsUnknown, "readback page says more comments follow but gives no cursor", nil
		}
		if !more {
			switch matches {
			case 0:
				return ObsNotApplied, "no comment carries the trailer", nil
			case 1:
				return ObsCommitted, "one comment carries the trailer", nil
			default:
				return ObsConflict, fmt.Sprintf("%d comments carry the trailer", matches), nil
			}
		}
	}
	return ObsUnknown, "comment history exceeded the page budget", nil
}

// checkEndpoint (goodqa#15168, Kimi K12): the bearer goes only to an https
// URL or to plain http on a literal loopback IP. The old prefix test let
// "http://127.0.0.1@other.host/" and "http://127.0.0.1.other.host/" through,
// sending the key to another host. Userinfo is refused outright, and the
// host is parsed, not prefix-matched. The URL itself is never echoed, since
// userinfo could hold a secret.
func checkEndpoint(raw string) error {
	bad := errors.New("goodqa endpoint must be https://host/... or http:// on a literal loopback IP, with no userinfo")
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.Host == "" || u.Hostname() == "" {
		return bad
	}
	if u.User != nil || strings.Contains(u.Host, "@") {
		return errors.New("goodqa endpoint must not carry userinfo")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if ip := net.ParseIP(u.Hostname()); ip != nil && ip.IsLoopback() {
			return nil
		}
	}
	return bad
}

// call performs one MCP tools/call and returns the tool's JSON payload, or a
// map with "__error__" set to the machine error code. Transport failures are
// errors (the broker records UNKNOWN). Responses are capped at 8 MB.
func (g *GoodQA) call(ctx context.Context, tool string, args map[string]any) (map[string]any, error) {
	if err := checkEndpoint(g.Endpoint); err != nil {
		return nil, err
	}
	keyBytes, err := os.ReadFile(g.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("goodqa key: %w", err)
	}
	key := strings.TrimSpace(string(keyBytes))
	if key == "" {
		return nil, errors.New("goodqa key file is empty")
	}
	payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args}})
	client := &http.Client{Timeout: 20 * time.Second}
	if g.Client != nil {
		c := *g.Client
		client = &c
	}
	// goodqa#15168: never follow a redirect. The bearer must reach only the
	// checked endpoint; a 3xx is reported as a transport failure below.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.Endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("authorization", "Bearer "+key)
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "application/json, text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8_000_001))
	if err != nil {
		return nil, err
	}
	if len(raw) > 8_000_000 {
		return nil, errors.New("goodqa response exceeded size budget")
	}
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return map[string]any{"__error__": "forbidden"}, nil
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, fmt.Errorf("goodqa HTTP %d redirect refused: the endpoint must answer directly", resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("goodqa HTTP %d", resp.StatusCode)
	}
	text := string(raw)
	if i := strings.LastIndex(text, "\ndata: "); i >= 0 || strings.HasPrefix(text, "data: ") {
		var last string
		for _, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(line, "data: ") {
				last = strings.TrimPrefix(line, "data: ")
			}
		}
		text = last
	}
	var env struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(text), &env); err != nil || len(env.Result.Content) == 0 {
		return nil, errors.New("goodqa returned an unexpected shape")
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(env.Result.Content[0].Text), &data); err != nil {
		if env.Result.IsError {
			// Validation refusals arrive as plain text ("MCP error -32602 ...").
			// Nothing was written: a definite refusal, not UNKNOWN.
			return map[string]any{"__error__": "validation_error", "__text__": firstN(env.Result.Content[0].Text, 160)}, nil
		}
		return nil, errors.New("goodqa tool payload is not a JSON object")
	}
	if env.Result.IsError {
		inner := data
		if d, ok := data["data"].(map[string]any); ok {
			inner = d
		}
		code := "error"
		if e, ok := inner["error"].(map[string]any); ok {
			if c, ok := e["code"].(string); ok && regexp.MustCompile(`^[a-z_]{1,40}$`).MatchString(c) {
				code = c
			}
		}
		return map[string]any{"__error__": code}, nil
	}
	return data, nil
}
