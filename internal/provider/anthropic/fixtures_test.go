package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fabric/internal/provider/anthropic/fakeapi"
)

type fixture struct {
	Scenario   string            `json:"scenario"`
	Provenance string            `json:"provenance"`
	Version    string            `json:"anthropic_version"`
	Status     int               `json:"status"`
	Headers    map[string]string `json:"headers"`
	Response   string            `json:"response"`
}

type expectation struct {
	Outcome    string   `json:"outcome"`
	Blocks     []string `json:"blocks"`
	StopReason string   `json:"stop_reason"`
	Input      int64    `json:"input"`
	Output     int64    `json:"output"`
	Tool       string   `json:"tool"`
	Signature  string   `json:"signature"`
	ErrorType  string   `json:"error_type"`
	Status     int      `json:"status"`
	Retry      bool     `json:"retry"`
	RetryAfter int      `json:"retry_after_s"`
}

func loadFixtures(t *testing.T) map[string]struct {
	fx  fixture
	exp expectation
} {
	t.Helper()
	files, _ := filepath.Glob("testdata/*.json")
	out := map[string]struct {
		fx  fixture
		exp expectation
	}{}
	for _, f := range files {
		if strings.HasSuffix(f, ".expect.json") {
			continue
		}
		var x fixture
		var e expectation
		b, _ := os.ReadFile(f)
		if err := json.Unmarshal(b, &x); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		eb, err := os.ReadFile(strings.TrimSuffix(f, ".json") + ".expect.json")
		if err != nil {
			t.Fatalf("%s has no expectation file", f)
		}
		_ = json.Unmarshal(eb, &e)
		if x.Provenance == "" || x.Version == "" {
			t.Fatalf("%s: provenance and anthropic-version must be recorded in fixture metadata", f)
		}
		out[x.Scenario] = struct {
			fx  fixture
			exp expectation
		}{x, e}
	}
	if len(out) < 8 {
		t.Fatalf("only %d fixtures found: the replay would pass vacuously", len(out))
	}
	return out
}

func replayClient(t *testing.T, st fakeapi.Step) (*Client, *fakeapi.Server, *[]time.Duration) {
	srv := fakeapi.New(st)
	t.Cleanup(srv.Close)
	var sl []time.Duration
	c, err := New(Config{BaseURL: ProductionBaseURL, HTTP: srv.HTTPClient(), Version: "v", Key: func(context.Context) (string, error) { return canary, nil },
		Retry:  RetryPolicy{MaxAttempts: 1, BaseBackoff: time.Second, MaxBackoff: time.Second, MaxRetryAfter: time.Minute},
		Sleep:  func(_ context.Context, d time.Duration) error { sl = append(sl, d); return nil },
		Jitter: func() float64 { return 0 }})
	if err != nil {
		t.Fatal(err)
	}
	return c, srv, &sl
}

func blockTypes(m *Message) []string {
	var out []string
	for _, b := range m.Content {
		var h struct{ Type string }
		_ = json.Unmarshal(b, &h)
		out = append(out, h.Type)
	}
	return out
}

func TestSSEFixtures(t *testing.T) {
	for name, f := range loadFixtures(t) {
		t.Run(name, func(t *testing.T) {
			c, _, _ := replayClient(t, fakeapi.Step{Status: f.fx.Status, Raw: f.fx.Response, Headers: f.fx.Headers})
			m, recs, err := c.Send(context.Background(), body(t), Hooks{})
			e := f.exp
			switch e.Outcome {
			case "ok":
				if err != nil {
					t.Fatal(err)
				}
				if strings.Join(blockTypes(m), ",") != strings.Join(e.Blocks, ",") || m.StopReason != e.StopReason || m.Usage.Input != e.Input || m.Usage.Output != e.Output {
					t.Fatalf("got blocks=%v stop=%s usage=%+v", blockTypes(m), m.StopReason, m.Usage)
				}
				if e.Tool != "" {
					tus, _ := m.ToolUses()
					if len(tus) != 1 || tus[0].Name != e.Tool || !json.Valid(tus[0].Input) {
						t.Fatalf("tool uses %+v", tus)
					}
				}
				if e.Signature != "" && !strings.Contains(string(m.Content[0]), e.Signature) {
					t.Fatal("signature lost")
				}
			case "stream_error":
				var se *StreamError
				if !errors.As(err, &se) || se.Type != e.ErrorType || m != nil {
					t.Fatalf("err=%v", err)
				}
			case "park":
				if !IsPark(err) {
					t.Fatalf("err=%v", err)
				}
			case "api_error":
				var ae *APIError
				if !errors.As(err, &ae) || ae.Status != e.Status || (ae.HasRetry && int(ae.RetryAfter.Seconds()) != e.RetryAfter) {
					t.Fatalf("err=%v", err)
				}
			default:
				t.Fatalf("unknown expectation %q", e.Outcome)
			}
			if len(recs) != 1 {
				t.Fatalf("attempts %d", len(recs))
			}
		})
	}
}

// Failing controls: a damaged recording must not replay as a success.
func TestSSEFixtureFailingControls(t *testing.T) {
	for name, f := range loadFixtures(t) {
		if f.exp.Outcome != "ok" {
			continue
		}
		cut := f.fx.Response[:len(f.fx.Response)-30]
		c, _, _ := replayClient(t, fakeapi.Step{Raw: cut})
		if m, _, err := c.Send(context.Background(), body(t), Hooks{}); err == nil || m != nil {
			t.Errorf("%s: truncated recording replayed as success", name)
		}
	}
	// Dropping the message_stop line of a good stream is also an error.
	ok := loadFixtures(t)["text"].fx.Response
	c, _, _ := replayClient(t, fakeapi.Step{Raw: strings.Replace(ok, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", "", 1)})
	if _, _, err := c.Send(context.Background(), body(t), Hooks{}); err == nil {
		t.Error("stream without message_stop accepted")
	}
}
