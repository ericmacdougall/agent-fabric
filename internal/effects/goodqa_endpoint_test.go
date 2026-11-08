package effects

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// goodqa#15168 (Kimi K12): the endpoint is parsed, not prefix-matched.
func TestCheckEndpoint(t *testing.T) {
	for _, ok := range []string{
		"https://goodqa.app/v1/mcp",
		"https://goodqa.app:8443/v1/mcp",
		"http://127.0.0.1:8080/v1/mcp",
		"http://127.0.0.2/v1/mcp",
		"http://[::1]:9000/v1/mcp",
	} {
		if err := checkEndpoint(ok); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"http://127.0.0.1@other.example/v1/mcp",    // the K12 reproducer: userinfo, other host
		"http://127.0.0.1:80@other.example/v1/mcp", // userinfo with a port-like suffix
		"https://user:secret@goodqa.app/v1/mcp",    // https with userinfo
		"https://@goodqa.app/v1/mcp",               // empty userinfo
		"http://127.0.0.1.other.example/v1/mcp",    // loopback-looking prefix, other host
		"http://localhost/v1/mcp",                  // a name, not a literal loopback IP
		"http://10.0.0.1/v1/mcp",                   // plain http off loopback
		"http://goodqa.app/v1/mcp",                 // plain http to the real host
		"ftp://127.0.0.1/v1/mcp",                   // other scheme
		"https:///v1/mcp",                          // no host
		"https:goodqa.app/v1/mcp",                  // opaque
		"",                                         // empty
		"HTTP://127.0.0.1@other.example/",          // scheme case does not reopen it
	} {
		err := checkEndpoint(bad)
		if err == nil {
			t.Errorf("%q accepted", bad)
			continue
		}
		if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "other.example") {
			t.Errorf("%q: the refusal echoes the URL: %v", bad, err)
		}
	}
}

func keyFileFor(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "member.key")
	if err := os.WriteFile(p, []byte("k12-test-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// The K12 reproducer end to end: a loopback-prefixed URL whose userinfo
// hides another host sends nothing, and the key never reaches that host.
func TestGoodQAUserinfoEndpointSendsNothing(t *testing.T) {
	var hits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(200)
	}))
	t.Cleanup(other.Close)
	host := strings.TrimPrefix(other.URL, "http://")
	g := &GoodQA{Endpoint: "http://127.0.0.1@" + host + "/v1/mcp", KeyFile: keyFileFor(t)}
	if _, err := g.call(context.Background(), "list_comments", map[string]any{}); err == nil {
		t.Fatal("a userinfo endpoint was accepted")
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the other host received %d request(s)", n)
	}
}

// A redirect is never followed, so the bearer cannot be carried to another
// endpoint (Go strips it only across domains, not across ports or schemes).
func TestGoodQARefusesRedirect(t *testing.T) {
	var leaked atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("authorization") != "" {
			leaked.Add(1)
		}
		w.WriteHeader(200)
	}))
	t.Cleanup(target.Close)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/v1/mcp", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(front.Close)
	for _, client := range []*http.Client{nil, front.Client()} {
		g := &GoodQA{Endpoint: front.URL + "/v1/mcp", KeyFile: keyFileFor(t), Client: client}
		_, err := g.call(context.Background(), "list_comments", map[string]any{})
		if err == nil || !strings.Contains(err.Error(), "redirect refused") {
			t.Errorf("client %v: a redirect was not refused: %v", client != nil, err)
		}
	}
	if n := leaked.Load(); n != 0 {
		t.Fatalf("the redirect target received the bearer %d time(s)", n)
	}
	if front.Client().CheckRedirect != nil {
		t.Fatal("call mutated the caller's http.Client")
	}
}
