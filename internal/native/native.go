// Package native wires one native-loop mission: grant validation, the host
// boundary check, the key source, the tools, the guard oracle, the spend
// ledger and the loop, and records the attempt through the ordinary
// commands so the reducer's event log stays the record.
package native

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"fabric/internal/agent"
	"fabric/internal/domain"
	"fabric/internal/host"
	"fabric/internal/hostid"
	"fabric/internal/policy"
	"fabric/internal/provider/anthropic"
	"fabric/internal/settle"
	"fabric/internal/store"
	"fabric/internal/tools"
)

// Spend ceilings fixed by the user's decisions (goodqa#15040 D-A; the month
// ceiling lowered to USD 20 for the first native missions, goodqa#15086). They are
// enforced in code: a grant cannot raise them.
const (
	monthCapMicros    = 20_000_000 // USD 20 a month
	jobCapMicros      = 5_000_000  // USD 5 per job
	liveTestCapMicros = 1_000_000  // USD 1 per live test
)

// The same ceilings, exported read-only for metrics.
const (
	MonthCapMicros    = monthCapMicros
	JobCapMicros      = jobCapMicros
	LiveTestCapMicros = liveTestCapMicros
)

// Grant is the file `fab mission run --grant` reads. It carries no
// secret: the key comes from KeyCommand (a secret-manager read).
type Grant struct {
	Provider string `json:"provider"`
	Mission  string `json:"mission"`
	Attempt  string `json:"attempt"`
	Ticket   string `json:"ticket"`

	Model     string                     `json:"model"`
	MaxTokens int                        `json:"max_tokens"`
	System    string                     `json:"system"`
	Prompt    string                     `json:"prompt"`
	Extra     map[string]json.RawMessage `json:"extra"`

	Worktree string `json:"worktree"`
	RunDir   string `json:"run_dir"`

	BaseURL          string   `json:"base_url"`
	AnthropicVersion string   `json:"anthropic_version"`
	KeyCommand       []string `json:"key_command"`

	Prices json.RawMessage `json:"prices"`
	Caps   struct {
		Month int64 `json:"month_usd_micros"`
		Job   int64 `json:"job_usd_micros"`
		Run   int64 `json:"run_usd_micros"`
	} `json:"caps"`
	LiveTest bool `json:"live_test"`

	MaxTurns       int   `json:"max_turns"`
	WallSeconds    int   `json:"wall_seconds"`
	MaxTotalTokens int64 `json:"max_total_tokens"`
	RestartCap     int   `json:"restart_cap"`

	ShellAllow       []string   `json:"shell_allow"`
	OracleHooks      [][]string `json:"oracle_hooks"`
	RequiredControls []string   `json:"required_controls"`
}

func LoadGrant(path string) (Grant, error) {
	var g Grant
	b, err := os.ReadFile(path)
	if err != nil {
		return g, err
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&g); err != nil {
		return g, fmt.Errorf("grant: %w", err)
	}
	return g, g.Validate()
}

func (g Grant) Validate() error {
	var p []string
	if g.Provider != "anthropic" {
		p = append(p, "provider must be anthropic")
	}
	if _, err := domain.ParseMissionID(g.Mission); err != nil {
		p = append(p, "mission id")
	}
	if _, err := domain.ParseAttemptID(g.Attempt); err != nil {
		p = append(p, "attempt id")
	}
	if g.Model == "" || g.MaxTokens <= 0 || g.Prompt == "" {
		p = append(p, "model, max_tokens and prompt are required (the grant names the model)")
	}
	if !filepath.IsAbs(g.Worktree) || !filepath.IsAbs(g.RunDir) {
		p = append(p, "worktree and run_dir must be absolute")
	}
	if g.BaseURL == "" || g.AnthropicVersion == "" || len(g.KeyCommand) == 0 {
		p = append(p, "base_url, anthropic_version and key_command are required")
	} else if g.BaseURL != APIBaseURL {
		p = append(p, "base_url must be exactly "+APIBaseURL)
	}
	if g.MaxTurns <= 0 || g.WallSeconds <= 0 {
		p = append(p, "max_turns and wall_seconds are required")
	}
	if g.Caps.Run <= 0 || g.Caps.Job <= 0 || g.Caps.Month <= 0 {
		p = append(p, "all three caps are required")
	}
	if g.Caps.Month > monthCapMicros || g.Caps.Job > jobCapMicros || g.Caps.Run > g.Caps.Job {
		p = append(p, "caps exceed the fixed ceilings (month 20 USD, job 5 USD, run <= job)")
	}
	if g.LiveTest && g.Caps.Run > liveTestCapMicros {
		p = append(p, "a live test is capped at 1 USD")
	}
	if len(p) > 0 {
		return errors.New("grant refused: " + strings.Join(p, "; "))
	}
	return nil
}

// Options carries what the caller resolved about this host.
type Options struct {
	Starter  tools.Starter
	Boundary tools.Boundary
	HTTP     *http.Client
}

// Receipt is the run summary printed for the live-test record.
type Receipt struct {
	Mission    string   `json:"mission"`
	Attempt    string   `json:"attempt"`
	Fence      int64    `json:"fence"`
	Outcome    string   `json:"outcome"`
	Note       string   `json:"note"`
	Turns      int      `json:"turns"`
	RequestIDs []string `json:"request_ids"`
	SpentMicro int64    `json:"spent_usd_micros"`
	CostLabel  string   `json:"cost_label"`
	Billing    string   `json:"billing_mode"`
	Starts     int      `json:"process_starts"`
}

func Run(ctx context.Context, st *store.Store, g Grant, o Options) (*Receipt, error) {
	// goodqa#15087 AC-5: the key goes to the Anthropic API only; checked
	// before anything else, so a wrong base_url never runs the key command.
	if err := checkBaseURL(g.BaseURL); err != nil {
		return nil, err
	}
	prices, err := agent.ParsePrices(g.Prices)
	if err != nil {
		return nil, err
	}
	// The host boundary comes first: nothing is sent to the provider, no key is
	// read, before the tools can be confined.
	if o.Starter == nil {
		return nil, errors.New("native: no host to run tools on")
	}
	if err := tools.RequireBoundary(o.Boundary); err != nil {
		return nil, err
	}
	starter := o.Starter
	// goodqa#15087: the start rule reads the mission and attempt before
	// SelfTest, the key command and any write; a refusal leaves no trace.
	plan, err := planStart(ctx, st, g)
	if err != nil {
		return nil, err
	}
	oracle := &policy.Oracle{Hooks: g.OracleHooks, Cwd: g.Worktree, Timeout: 20 * time.Second}
	if err := oracle.SelfTest(ctx); err != nil {
		return nil, err
	}
	db := &agent.DB{Pool: st.Pool()}
	if plan.c == caseReplay {
		// Already finished: settle the recorded outcome under native T1 and
		// send nothing; no key is needed.
		led := &agent.Ledger{St: st, Attempt: domain.AttemptID(g.Attempt), Fence: plan.fence}
		return finishNative(ctx, st, db, led, g, plan.fence, agent.Outcome{Kind: plan.outcome, Note: plan.note})
	}
	lockPath := plan.lockPath
	if plan.c != caseResume {
		if lockPath, err = LockPath(domain.AttemptID(g.Attempt)); err != nil {
			return nil, err
		}
	}
	release, err := ensureLocks(ctx, db, domain.AttemptID(g.Attempt), lockPath)
	if err != nil {
		return nil, err
	}
	defer release()
	if plan.c == caseResume {
		// V1: the first heartbeat of a resume needs an unexpired lease.
		if err := st.HeartbeatLive(ctx, domain.AttemptID(g.Attempt), plan.fence); err != nil {
			return nil, fmt.Errorf("%w: resume of %s: %v", ErrStartRefused, g.Attempt, err)
		}
	}
	var allow []*regexp.Regexp
	for _, pat := range g.ShellAllow {
		re, err := regexp.Compile(pat)
		if err != nil {
			return nil, fmt.Errorf("shell_allow %q: %w", pat, err)
		}
		allow = append(allow, re)
	}

	keySrc, keys, err := newKeyRing(g.KeyCommand, 5*time.Minute)
	if err != nil {
		return nil, err
	}
	// The first fetch checks the key before anything is started; every value
	// the source returns, a rotated one included, is redacted from tool output.
	if _, err := keySrc(ctx); err != nil {
		return nil, err
	}
	cl, err := anthropic.New(anthropic.Config{BaseURL: g.BaseURL, Version: g.AnthropicVersion, Key: keySrc, HTTP: o.HTTP})
	if err != nil {
		return nil, err
	}

	// The month cap is keyed on database time, never the host clock, so a
	// skewed or faked host clock cannot open a fresh month (goodqa#15124).
	// Read before the attempt starts, so a failure leaves nothing to reconcile.
	month, err := st.DBMonth(ctx)
	if err != nil {
		return nil, fmt.Errorf("native: month key: %w", err)
	}

	fence := plan.fence
	if plan.c != caseResume {
		if fence, err = startNative(ctx, st, g, plan.c == caseNew, lockPath); err != nil {
			return nil, err
		}
	}
	tok := make([]byte, 16)
	_, _ = rand.Read(tok)
	hg := host.Grant{Run: domain.AttemptID(g.Attempt), Mission: domain.MissionID(g.Mission), Fence: fence, Model: g.Model, Effort: "n/a",
		Rung: "native", TierTableSHA: "native", Profile: "P1", MaxTurns: g.MaxTurns, MaxBudgetUSD: float64(g.Caps.Run) / 1e6,
		Wall: time.Duration(g.WallSeconds) * time.Second, Worktree: g.Worktree, RunDir: g.RunDir, Token: hex.EncodeToString(tok)}
	conf := &tools.HostConfiner{Host: starter, Base: hg}
	rd, err := tools.NewRead(g.Worktree, 64<<10, 10*time.Second)
	if err != nil {
		return nil, err
	}
	toolset := []agent.Tool{rd,
		&tools.Grep{C: conf, Root: g.Worktree, Timeout: 20 * time.Second, MaxOutput: 32 << 10},
		&tools.Shell{C: conf, Allow: allow, Timeout: 30 * time.Second, MaxOutput: 32 << 10},
	}

	led := &agent.Ledger{St: st, Attempt: domain.AttemptID(g.Attempt), Fence: fence, TTL: time.Duration(g.WallSeconds+600) * time.Second,
		Caps: []agent.Cap{
			{Resource: agent.SpendResource("month", month), Total: g.Caps.Month},
			{Resource: agent.SpendResource("job", g.Mission), Total: g.Caps.Job},
			{Resource: agent.SpendResource("run", g.Attempt), Total: g.Caps.Run},
		}}
	loop, err := agent.New(agent.Config{
		DB: db, Ledger: led, Provider: &anthropic.Provider{C: cl}, Tools: toolset, Guard: oracle,
		Redact:  keys,
		Attempt: domain.AttemptID(g.Attempt), Fence: fence,
		Model: g.Model, MaxTokens: g.MaxTokens, System: g.System, Extra: g.Extra, Prompt: g.Prompt,
		MaxTurns: g.MaxTurns, MaxTotalTokens: g.MaxTotalTokens, Wall: time.Duration(g.WallSeconds) * time.Second, RestartCap: restartCap(g.RestartCap),
		Prices: prices, BillingMode: agent.BillingAPIKey, Pin: g.pin(),
	})
	if err != nil {
		return nil, err
	}
	lctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stop := heartbeat(lctx, st, domain.AttemptID(g.Attempt), fence, cancel)
	defer stop()
	out, err := loop.Run(lctx)
	if cause := context.Cause(lctx); cause != nil && lctx.Err() != nil && ctx.Err() == nil {
		return nil, cause // the heartbeat stopped the loop: settle nothing
	}
	if err != nil {
		return nil, err
	}
	return finishNative(ctx, st, db, led, g, fence, out)
}

// startNative registers the mission and starts the attempt through the
// ordinary commands (so the reducer's event log stays the record), after the
// start rule chose caseNew (admit first) or caseStartAdmitted (the mission
// is already admitted: no second admit). It runs with the flock held.
func startNative(ctx context.Context, st *store.Store, g Grant, admit bool, lockPath string) (int64, error) {
	// The native runner creates anthropic-api missions only (goodqa#15087,
	// main WAL w5a7fb2daf13); an existing mission of another provider is
	// refused here. (skipPrecheck: test only; the reducer and the database
	// must then refuse on their own.)
	if !skipPrecheck {
		if err := st.CreateMissionSpec(ctx, store.MissionSpec{ID: domain.MissionID(g.Mission), Name: "native " + g.Mission, Ticket: g.Ticket, Priority: 100,
			Source: "ticket:" + g.Ticket, Provider: domain.ProviderAnthropicAPI}); err != nil {
			return 0, err
		}
	}
	key := func(s string) domain.CommandID { return domain.CommandID("native-" + shortID(g.Attempt) + "-" + s) }
	if admit {
		if _, err := st.Execute(ctx, "native-runner", key("admit"), domain.Command{Kind: domain.CmdAdmitMission, Mission: domain.MissionID(g.Mission)}); err != nil {
			return 0, fmt.Errorf("admit: %w", err)
		}
	}
	resp, err := st.Execute(ctx, "native-runner", key("start"), domain.Command{Kind: domain.CmdStartAttempt, Mission: domain.MissionID(g.Mission),
		Attempt: domain.AttemptID(g.Attempt), Host: hostid.Default(), Role: "writer", Path: domain.ProviderAnthropicAPI, LockPath: lockPath})
	if err != nil {
		return 0, fmt.Errorf("start attempt: %w", err)
	}
	fence := resp.Events[0].Fence
	if _, err := st.Execute(ctx, "native-runner", key("ack"), domain.Command{Kind: domain.CmdAckStart, Mission: domain.MissionID(g.Mission),
		Attempt: domain.AttemptID(g.Attempt), Fence: fence}); err != nil {
		return 0, fmt.Errorf("ack start: %w", err)
	}
	return fence, nil
}

func shortID(s string) string {
	if len(s) > 24 {
		return s[len(s)-24:]
	}
	return s
}

// ErrNotSettled: native T1 found nothing safe to settle; the attempt is
// left RUNNING for its lease to expire into reconcile.
var ErrNotSettled = errors.New("native T1: nothing settled, the attempt is left to lease expiry")

// t1Outcome is the native T1 rule (goodqa#15087, main WAL wbe444749c14):
// OutcomeDone is a checkpoint only with a recorded final assistant turn;
// parked, budget_exhausted, max_turns and wall_exhausted settle blocked;
// anything else, an unknown or unreadable outcome included, settles
// nothing.
func t1Outcome(ctx context.Context, db *agent.DB, attempt domain.AttemptID, out agent.Outcome) (string, error) {
	switch out.Kind {
	case agent.OutcomeDone:
		ok, err := finalTurn(ctx, db, attempt)
		if err != nil {
			return "", fmt.Errorf("%w: reading the final turn: %v", ErrNotSettled, err)
		}
		if !ok {
			return "", fmt.Errorf("%w: outcome done with no recorded final assistant turn", ErrNotSettled)
		}
		return "checkpoint", nil
	case agent.OutcomeParked, agent.OutcomeBudgetExhausted, agent.OutcomeMaxTurns, agent.OutcomeWallExhausted:
		return "blocked", nil
	}
	return "", fmt.Errorf("%w: unknown outcome %q", ErrNotSettled, out.Kind)
}

// finalTurn: the last recorded message is an assistant turn with no tool
// call, the turn the loop ended on.
func finalTurn(ctx context.Context, db *agent.DB, attempt domain.AttemptID) (bool, error) {
	msgs, err := db.Messages(ctx, attempt)
	if err != nil || len(msgs) == 0 {
		return false, err
	}
	last := msgs[len(msgs)-1]
	if last.Role != "assistant" {
		return false, nil
	}
	calls, err := db.ToolCalls(ctx, attempt)
	if err != nil {
		return false, err
	}
	for _, c := range calls {
		if c.MsgIdx == last.Idx {
			return false, nil
		}
	}
	return true, nil
}

// Test seams for the finish retry (goodqa#15087 AC-7); nil in production.
// afterFinishExec sees each finish result and may replace its error, so a
// test can lose the reply of a committed finish; settleClock runs the
// backoff on fake time.
var (
	afterFinishExec func(resp store.Response, err error) error
	settleClock     settle.Clock
)

func finishNative(ctx context.Context, st *store.Store, db *agent.DB, led *agent.Ledger, g Grant, fence int64, out agent.Outcome) (*Receipt, error) {
	outcome, err := t1Outcome(ctx, db, domain.AttemptID(g.Attempt), out)
	if err != nil {
		return nil, err
	}
	// goodqa#15087 AC-7: the same finish-with-retry as the claude-code
	// settle; the command id makes a retry after a lost reply a replay.
	cmd := domain.Command{Kind: domain.CmdFinishAttempt, Mission: domain.MissionID(g.Mission), Attempt: domain.AttemptID(g.Attempt), Fence: fence, Outcome: outcome}
	if err := settle.Finish(ctx, settle.Options{Clock: settleClock}, func(ctx context.Context) error {
		resp, err := st.Execute(ctx, "native-runner", domain.CommandID("native-"+shortID(g.Attempt)+"-finish"), cmd)
		if afterFinishExec != nil {
			err = afterFinishExec(resp, err)
		}
		return err
	}); err != nil {
		return nil, fmt.Errorf("finish attempt: %w", err)
	}
	r := &Receipt{Mission: g.Mission, Attempt: g.Attempt, Fence: fence, Outcome: out.Kind, Note: out.Note, Turns: out.Turns, Billing: agent.BillingAPIKey, CostLabel: agent.LabelAPIEstimate}
	run, err := db.Run(ctx, domain.AttemptID(g.Attempt))
	if err != nil {
		return r, err
	}
	r.Starts = run.Starts
	rows, err := st.Pool().Query(ctx, `SELECT unnest(request_ids) FROM agent_messages WHERE attempt_id = $1 AND role = 'assistant' ORDER BY idx`, g.Attempt)
	if err != nil {
		return r, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		r.RequestIDs = append(r.RequestIDs, id)
	}
	spent, err := led.Spent(ctx, agent.SpendResource("run", g.Attempt))
	r.SpentMicro = spent
	return r, err
}

// restartCap defaults to 5 starts so one crash does not park the attempt.
func restartCap(n int) int {
	if n <= 0 {
		return 5
	}
	return n
}

// pin hashes the parts of the grant that bound what a resumed run may do.
func (g Grant) pin() string {
	b, _ := json.Marshal(struct {
		Allow  []string
		Hooks  [][]string
		Caps   any
		Wall   int
		Turns  int
		Tokens int64
	}{g.ShellAllow, g.OracleHooks, g.Caps, g.WallSeconds, g.MaxTurns, g.MaxTotalTokens})
	return string(b)
}
