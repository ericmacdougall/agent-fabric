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
	"fabric/internal/harness"
	hnative "fabric/internal/harness/native"
	"fabric/internal/host"
	"fabric/internal/hostid"
	"fabric/internal/policy"
	"fabric/internal/provider/anthropic"
	"fabric/internal/reviewgate"
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
	// KeyCheck is "shape" (default) or "authority": see anthropic.KeyCheck.
	KeyCheck string `json:"key_check,omitempty"`

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
	// ReviewGate configures the durable tool review gate. It takes effect only
	// when FABRIC_REVIEW_GATE=1 (Options.ReviewGate); with the flag off a grant
	// that sets it is refused rather than silently running ungated.
	ReviewGate ReviewGateConfig `json:"review_gate,omitempty"`
	// Compaction opts into context compaction (goodqa#15361 N3); Betas lists
	// extra anthropic-beta values from a closed allowlist.
	Compaction *CompactionGrant `json:"compaction,omitempty"`
	Betas      []string         `json:"betas,omitempty"`
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
	if !anthropic.KeyCheck(g.KeyCheck).Valid() {
		p = append(p, "key_check must be shape or authority")
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
	if err := validateReviewGateConfig(g.ReviewGate); err != nil {
		p = append(p, err.Error())
	}
	p = append(p, validateCompaction(g)...)
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
	// ReviewGate turns on the durable tool review gate (FABRIC_REVIEW_GATE=1
	// in cmd/fab). Off, Run behaves exactly as before the gate existed and
	// needs no review DSN. On, every tool is gated and ReviewDSN is required.
	ReviewGate bool
	// ToolExitCleanup verifies descendant cleanup after every tool exit
	// (FABRIC_TOOL_EXIT_CLEANUP=1 in cmd/fab; default off). See
	// tools.HostConfiner.ExitCleanup.
	ToolExitCleanup bool
	// ReviewDSN authenticates the separate trusted app-role review pool. It
	// must never be the runner DSN or be inherited by a tool worker.
	ReviewDSN string
	// ReviewClose releases an optional owned file reference after all Run and
	// review-pool use. The caller that resolved Options must defer Close.
	ReviewClose func() error
	// Sink, when set, receives the run as harness events (turns, messages,
	// tool calls, usage, one final). It observes only: the loop, its tools and
	// its guard are unchanged, and a sink that cannot record stops the run
	// (see internal/harness/native). Used by `fab harness run`.
	Sink harness.Sink
}

// Close releases caller-owned review credential transport. References are
// idempotent; copied Options must not be closed before their Run finishes.
func (o Options) Close() error {
	if o.ReviewClose != nil {
		return o.ReviewClose()
	}
	return nil
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
	// KeyCheck records an authority check as status and sha256 prefix only.
	KeyCheck string `json:"key_check,omitempty"`
}

func Run(ctx context.Context, st *store.Store, g Grant, o Options) (rc *Receipt, rerr error) {
	// goodqa#15087 AC-5: the key goes to the Anthropic API only; checked
	// before anything else, so a wrong base_url never runs the key command.
	if err := checkBaseURL(g.BaseURL); err != nil {
		return nil, err
	}
	// Load validates, but an embedder can build a Grant in code; Run checks
	// the caps and fields itself before any key, request or write
	// (goodqa#15235 S4).
	if err := g.Validate(); err != nil {
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
	// goodqa#15283: the grant's worktree is canonicalized and pinned before
	// the start rule and any Execute, so a refused path never burns the keyed
	// start command. From here on only the canonical path is used.
	wt, err := canonicalWorktree(g.Worktree)
	if err != nil {
		return nil, err
	}
	// goodqa#15087: the start rule reads the mission and attempt before
	// SelfTest, the key command and any write; a refusal leaves no trace.
	plan, err := planStart(ctx, st, g, wt.Path())
	if err != nil {
		return nil, err
	}
	oracle := nativeOracle(g, wt)
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
	var reviewStore *reviewgate.PostgresStore
	if o.ReviewGate {
		if o.ReviewDSN == "" {
			return nil, errors.New("native: review DSN is required (set FABRIC_REVIEW_DSN to the app-role database)")
		}
		if reviewStore, err = reviewgate.Open(ctx, o.ReviewDSN); err != nil {
			return nil, fmt.Errorf("native: open app-role review store: %w", err)
		}
		defer reviewStore.Close()
	} else if g.ReviewGate.set() {
		return nil, errors.New("native: grant sets review_gate but FABRIC_REVIEW_GATE is not 1; refusing to run its tools ungated")
	}
	lockPath := plan.lockPath
	if plan.c != caseResume {
		if lockPath, err = LockPath(domain.AttemptID(g.Attempt)); err != nil {
			return nil, err
		}
	}
	if beforeLocks != nil {
		beforeLocks()
	}
	release, err := ensureLocks(ctx, db, domain.AttemptID(g.Attempt), lockPath)
	if err != nil {
		return nil, err
	}
	defer release()
	// goodqa#15283 red-team K3: planStart read the binding before the flock;
	// read it again now that the flock is held, before the key command and
	// any write. (SelfTest has run by now; it reads and writes nothing.)
	if err := recheckWorktree(ctx, st, g, wt.Path()); err != nil {
		return nil, err
	}

	// A resumed attempt is covered before key access and setup. A new attempt
	// starts heartbeating immediately after its keyed start/ack below.
	fence := plan.fence
	lctx := ctx
	var cancel context.CancelCauseFunc
	stopHeartbeat := func() {}
	defer func() {
		if cancel != nil {
			cancel(nil)
		}
		stopHeartbeat()
	}()
	startHeartbeat := func() error {
		if err := st.HeartbeatLive(ctx, domain.AttemptID(g.Attempt), fence); err != nil {
			return err
		}
		lctx, cancel = context.WithCancelCause(ctx)
		stopHeartbeat = heartbeat(lctx, st, domain.AttemptID(g.Attempt), fence, cancel)
		return nil
	}
	if plan.c == caseResume {
		// V1: the first heartbeat of a resume needs an unexpired lease.
		if err := startHeartbeat(); err != nil {
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

	keySrc, keys, err := newKeyRing(g.KeyCommand, 5*time.Minute, anthropic.KeyCheck(g.KeyCheck), o.HTTP)
	if err != nil {
		return nil, err
	}
	// The first fetch checks a new run's key before anything is started; every
	// value the source returns, a rotated one included, is redacted from tools.
	if _, err := keySrc(lctx); err != nil {
		return nil, err
	}
	loopExtra, betas, err := compactionExtra(g)
	if err != nil {
		return nil, err
	}
	cl, err := anthropic.New(anthropic.Config{BaseURL: g.BaseURL, Version: g.AnthropicVersion, Key: keySrc, HTTP: o.HTTP, Vouched: &keys.vouched, Betas: betas})
	if err != nil {
		return nil, err
	}

	// The month cap is keyed on database time, never the host clock, so a
	// skewed or faked host clock cannot open a fresh month (goodqa#15124).
	// Read before a new attempt starts, so a failure there leaves nothing to
	// reconcile. nativeProvider refreshes it before every HTTP attempt, so a
	// run that spans a month boundary charges the month of each request.
	month, err := monthSource(lctx, st)
	if err != nil {
		return nil, fmt.Errorf("native: month key: %w", err)
	}

	if plan.c != caseResume {
		if fence, err = startNative(ctx, st, g, plan.c == caseNew, lockPath, wt.Path()); err != nil {
			return nil, err
		}
		if err := startHeartbeat(); err != nil {
			return nil, err
		}
	}
	tok := make([]byte, 16)
	_, _ = rand.Read(tok)
	hg := host.Grant{Run: domain.AttemptID(g.Attempt), Mission: domain.MissionID(g.Mission), Fence: fence, Model: g.Model, Effort: "n/a",
		Rung: "native", TierTableSHA: "native", Profile: "P1", MaxTurns: g.MaxTurns, MaxBudgetUSD: float64(g.Caps.Run) / 1e6,
		Wall: time.Duration(g.WallSeconds) * time.Second, RunDir: g.RunDir, Token: hex.EncodeToString(tok)}
	stateDir, err := StateDir()
	if err != nil {
		return nil, err
	}
	reapOrphanSpill(lctx, st, stateDir, g.Attempt)
	toolset, tres, err := nativeToolsetOpts(wt, starter, hg, o.ToolExitCleanup, allow,
		toolsetOpts{FileTools: fileToolsEnabled(o.ReviewGate, g.ReviewGate), StateDir: stateDir, Attempt: g.Attempt})
	if err != nil {
		return nil, err
	}
	defer tres.close()
	// Every tool refuses to start once the attempt lease is stale or expired.
	for i, tool := range toolset {
		toolset[i] = freshTool{Tool: tool, st: st, attempt: domain.AttemptID(g.Attempt), fence: fence}
	}
	if o.ReviewGate {
		if toolset, err = wrapReviewTools(toolset, st, reviewStore, g, keys); err != nil {
			return nil, err
		}
	}

	led := &agent.Ledger{St: st, Attempt: domain.AttemptID(g.Attempt), Fence: fence, TTL: time.Duration(g.WallSeconds+600) * time.Second,
		Caps: []agent.Cap{
			{Resource: agent.SpendResource("month", month), Total: g.Caps.Month},
			{Resource: agent.SpendResource("job", g.Mission), Total: g.Caps.Job},
			{Resource: agent.SpendResource("run", g.Attempt), Total: g.Caps.Run},
		}}
	provider := &nativeProvider{base: &anthropic.Provider{C: cl}, st: st, ledger: led, attempt: domain.AttemptID(g.Attempt), fence: fence}
	var loopProvider agent.Provider = provider
	var loopOut agent.Outcome
	if o.Sink != nil {
		em, err := harness.NewEmitter(hnative.Name, g.Attempt, o.Sink, harness.ChainRedactor(keys, harness.PatternRedactor()))
		if err != nil {
			return nil, err
		}
		obs := hnative.NewObserver(em)
		loopProvider, toolset = obs.Provider(provider), obs.Tools(toolset)
		defer func() {
			obs.Finish(context.WithoutCancel(ctx), loopOut, rerr, errors.Is(context.Cause(ctx), context.Canceled))
		}()
	}
	loop, err := agent.New(agent.Config{
		DB: db, Ledger: led, Provider: loopProvider, Tools: toolset, Guard: oracle,
		Redact:  keys,
		Attempt: domain.AttemptID(g.Attempt), Fence: fence,
		Model: g.Model, MaxTokens: g.MaxTokens, System: g.System, Extra: loopExtra, Prompt: g.Prompt,
		Compaction: g.Compaction.agentConfig(),
		MaxTurns:   g.MaxTurns, MaxTotalTokens: g.MaxTotalTokens, Wall: time.Duration(g.WallSeconds) * time.Second, RestartCap: restartCap(g.RestartCap),
		Prices: prices, BillingMode: agent.BillingAPIKey, Pin: g.pin(o.ReviewGate),
	})
	if err != nil {
		return nil, err
	}
	out, err := loop.Run(lctx)
	for {
		if cause := context.Cause(lctx); cause != nil && lctx.Err() != nil && ctx.Err() == nil {
			return nil, cause // the heartbeat stopped the loop: settle nothing
		}
		if err != nil {
			return nil, err
		}
		if out.Kind != agent.OutcomeApprovalRequired {
			break
		}
		// Only a gated tool can pause; without a review store there is nothing
		// that could wake the call, so settle nothing.
		if reviewStore == nil || out.Review == nil || out.Review.RequestHash == "" {
			return nil, fmt.Errorf("native: malformed durable review pause")
		}
		// The flock, attempt fence and heartbeat remain held across the wait.
		// Approval is only a wake hint: replaying the same Loop revalidates the
		// exact PENDING call, current trusted state and live approval before use.
		if err := reviewStore.WaitForReviewDecision(lctx, out.Review.RequestHash); err != nil {
			if cause := context.Cause(lctx); cause != nil && lctx.Err() != nil && ctx.Err() == nil {
				return nil, cause
			}
			return nil, fmt.Errorf("native: wait for review decision: %w", err)
		}
		if err := st.HeartbeatLive(lctx, domain.AttemptID(g.Attempt), fence); err != nil {
			return nil, fmt.Errorf("native: resume after review: %w", err)
		}
		out, err = loop.Resume(lctx)
	}
	loopOut = out
	rcpt, ferr := finishNative(ctx, st, db, led, g, fence, out)
	if ferr == nil {
		tres.removeSpill()
	}
	if rcpt != nil {
		rcpt.KeyCheck = keys.lastCheck()
	}
	return rcpt, ferr
}

// nativeOracle is the run's guard oracle: its hooks see the canonical
// worktree as cwd, and every hook exec first checks the pin (goodqa#15283).
func nativeOracle(g Grant, wt *tools.WorktreePin) *policy.Oracle {
	return &policy.Oracle{Hooks: g.OracleHooks, Cwd: wt.Path(), Timeout: 20 * time.Second, Precheck: wt.Check}
}

// nativeToolset builds read, grep and shell on the canonical worktree, each
// refusing with ErrWorktreeMoved once the pin no longer holds
// (goodqa#15283 AC-5). The host grant's working directory is the canonical
// path too, whatever the caller set. It has no state directory: no spill, no
// edit or write.
func nativeToolset(wt *tools.WorktreePin, starter tools.Starter, hg host.Grant, exitCleanup bool, allow []*regexp.Regexp) ([]agent.Tool, error) {
	ts, _, err := nativeToolsetOpts(wt, starter, hg, exitCleanup, allow, toolsetOpts{})
	return ts, err
}

// toolsetOpts are the parts of the toolset that need the runner's state
// directory or the review gate (goodqa#15361).
type toolsetOpts struct {
	// FileTools adds edit and write. Set only by fileToolsEnabled.
	FileTools bool
	// StateDir and Attempt locate the runner-only spill and pre-image
	// directories. An empty StateDir disables both.
	StateDir string
	Attempt  string
}

// toolResources are the runner-only directories a toolset holds open.
type toolResources struct {
	spill, preimage *tools.RunnerDir
}

func (r *toolResources) close() {
	if r == nil {
		return
	}
	if r.spill != nil {
		r.spill.Close()
	}
	if r.preimage != nil {
		r.preimage.Close()
	}
}

// removeSpill deletes the attempt's spill directory once the attempt has
// finished. Pre-image copies stay: they are what makes an untracked
// overwrite reversible.
func (r *toolResources) removeSpill() {
	if r != nil && r.spill != nil {
		r.spill.Remove()
		r.spill = nil
	}
}

// fileToolsEnabled is the gate rule (goodqa#15361 F1): edit and write exist
// only when the operator flag is on AND the grant sets review_gate.mode
// explicitly to evaluate or enforce. Shadow, an unset mode and a missing flag
// leave them absent, so a model cannot even name them.
func fileToolsEnabled(reviewGate bool, c ReviewGateConfig) bool {
	return reviewGate && (c.Mode == string(reviewgate.Evaluate) || c.Mode == string(reviewgate.Enforce))
}

func nativeToolsetOpts(wt *tools.WorktreePin, starter tools.Starter, hg host.Grant, exitCleanup bool, allow []*regexp.Regexp, o toolsetOpts) (_ []agent.Tool, res *toolResources, err error) {
	hg.Worktree = wt.Path()
	conf := &tools.HostConfiner{Host: starter, Base: hg, ExitCleanup: exitCleanup, Pin: wt}
	rd, err := tools.NewRead(wt.Path(), 64<<10, 10*time.Second)
	if err != nil {
		return nil, nil, err
	}
	rd.Pin = wt
	res = &toolResources{}
	defer func() {
		if err != nil {
			res.close()
		}
	}()
	if o.StateDir != "" {
		if res.spill, err = tools.OpenRunnerDir(filepath.Join(o.StateDir, "spill", o.Attempt), 16, 64<<20); err != nil {
			return nil, nil, fmt.Errorf("native: spill directory: %w", err)
		}
		conf.Spill, rd.Spill = res.spill, res.spill
	}
	out := []agent.Tool{rd,
		&tools.Grep{C: conf, Root: wt.Path(), Timeout: 20 * time.Second, MaxOutput: 32 << 10, Pin: wt},
		&tools.Shell{C: conf, Allow: allow, Timeout: 30 * time.Second, MaxOutput: 32 << 10},
	}
	if o.FileTools {
		var keeper *tools.PreimageKeeper
		// edit and write are labelled Reversible: they never run without the
		// pre-image store that makes that true (goodqa#15361 F1).
		if o.StateDir == "" {
			return nil, nil, errors.New("native: edit and write need the runner state directory for pre-image copies")
		}
		// A keep dir: copies are never evicted; at 256 copies or 256 MiB the
		// call is refused with ErrPreimageStoreFull (F2). Only pre-* counts.
		dir, err := tools.OpenKeepDir(filepath.Join(o.StateDir, "preimage", o.Attempt), "pre-", 256, 256<<20)
		if err != nil {
			return nil, nil, fmt.Errorf("native: pre-image directory: %w", err)
		}
		res.preimage = dir
		keeper = &tools.PreimageKeeper{Dir: dir}
		// The gitdir is recorded once per attempt in the runner-only store and
		// a resume uses that record, never .git (F4). On any error (a replaced
		// gitdir included) Git stays nil and every existing target is copied.
		if g, gerr := tools.AttemptGitRef(dir, wt.Path()); gerr == nil {
			keeper.Git = g
		}
		out = append(out, tools.NewEdit(rd, wt, keeper), tools.NewWrite(rd, wt, keeper))
	}
	return out, res, nil
}

// reapOrphanSpill removes spill directories of attempts that are not running
// (R2-c). The current attempt is always kept.
func reapOrphanSpill(ctx context.Context, st *store.Store, stateDir, current string) {
	rows, err := st.Pool().Query(ctx, `SELECT id FROM attempts WHERE state IN ('LEASED','PREPARING','RUNNING')`)
	if err != nil {
		return // never reap on a failed read
	}
	defer rows.Close()
	live := map[string]bool{current: true}
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			live[id] = true
		}
	}
	if rows.Err() != nil {
		return
	}
	tools.ReapOrphanDirs(filepath.Join(stateDir, "spill"), func(n string) bool { return live[n] })
}

// startNative registers the mission and starts the attempt through the
// ordinary commands (so the reducer's event log stays the record), after the
// start rule chose caseNew (admit first) or caseStartAdmitted (the mission
// is already admitted: no second admit). It runs with the flock held.
// worktree is the canonical, pinned worktree the attempt is bound to
// (goodqa#15283), never the grant's own spelling.
func startNative(ctx context.Context, st *store.Store, g Grant, admit bool, lockPath, worktree string) (int64, error) {
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
		Attempt: domain.AttemptID(g.Attempt), Host: hostid.Default(), Role: "writer", Path: domain.ProviderAnthropicAPI, LockPath: lockPath,
		Worktree: worktree})
	if err != nil {
		return 0, fmt.Errorf("start attempt: %w", err)
	}
	// A replayed start means another runner already started this attempt
	// with the same keyed command; reusing its fence would run the attempt
	// twice (goodqa#15235 S4, from the Kimi 6ac28d9 patch).
	if resp.Replayed || len(resp.Events) == 0 {
		return 0, fmt.Errorf("%w: attempt %s was created concurrently", domain.ErrInvalidTransition, g.Attempt)
	}
	fence := resp.Events[0].Fence
	ack, err := st.Execute(ctx, "native-runner", key("ack"), domain.Command{Kind: domain.CmdAckStart, Mission: domain.MissionID(g.Mission),
		Attempt: domain.AttemptID(g.Attempt), Fence: fence})
	if err != nil {
		return 0, fmt.Errorf("ack start: %w", err)
	}
	if ack.Replayed {
		return 0, fmt.Errorf("%w: attempt %s acknowledgement was created concurrently", domain.ErrInvalidTransition, g.Attempt)
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
// With the review gate off the pin bytes are exactly those of a build without
// the gate; with it on, the gate mode and safe tools are pinned too, so a
// resume under a different gate configuration is refused.
func (g Grant) pin(reviewGate bool) string {
	review := ""
	if reviewGate {
		review = reviewGatePin(g.ReviewGate)
	}
	b, _ := json.Marshal(struct {
		Allow      []string
		Hooks      [][]string
		Caps       any
		Wall       int
		Turns      int
		Tokens     int64
		Review     string           `json:",omitempty"`
		Compaction *CompactionGrant `json:",omitempty"`
		Betas      []string         `json:",omitempty"`
	}{g.ShellAllow, g.OracleHooks, g.Caps, g.WallSeconds, g.MaxTurns, g.MaxTotalTokens, review, g.Compaction, g.Betas})
	return string(b)
}

// monthSource names the month the month cap is keyed on: database time,
// never the host clock (goodqa#15124). A test replaces it to prove Run
// keys the cap on what it returns.
var monthSource = func(ctx context.Context, st *store.Store) (string, error) { return st.DBMonth(ctx) }
