package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"fabric/internal/domain"
)

// Config wires one attempt's loop. Model, prices and caps come from the
// grant and configuration; none are defaulted here.
type Config struct {
	DB       *DB
	Ledger   *Ledger
	Provider Provider
	Tools    []Tool
	Guard    Guard
	Redact   Redactor

	Attempt domain.AttemptID
	Fence   int64

	Model     string
	MaxTokens int // per request, derived by the grant so the hold fits the cap
	System    string
	Extra     map[string]json.RawMessage
	Prompt    string

	MaxTurns       int
	MaxTotalTokens int64 // input+output across the attempt; 0 = unlimited
	// MaxServerToolUses bounds server-tool uses per response and is held at
	// its price before each request. 0 means none are allowed: a response
	// that reports any is settled in full, then ends the run.
	MaxServerToolUses int64
	Wall              time.Duration // attempt wall clock from first start
	RestartCap        int
	MaxResultBytes    int

	Prices      Prices
	BillingMode string
	// Pin is extra configuration the caller wants a resume to match (shell
	// allowlist, oracle hooks, caps); it joins the config hash.
	Pin string

	// Compaction is the validated grant's compaction setting (goodqa#15361 N3).
	Compaction CompactionConfig

	// Crash is a test seam called at named points; production leaves it nil.
	Crash func(point string)
}

// Outcome is how a run ended. Review is typed control metadata; Note remains
// the human/tool-facing rendering and is never authoritative for resumption.
type Outcome struct {
	Kind   string
	Note   string
	Turns  int
	Review *ReviewPause
}

type Loop struct {
	c       Config
	tools   map[string]Tool
	starts  int
	settleE error
}

// ErrSubscriptionBilling refuses a loop configured for subscription
// billing. Subscription work runs as a claude-code mission under fabd.
var ErrSubscriptionBilling = errors.New("agent: subscription billing is refused: the native loop runs on an API key only; subscription work runs as a claude-code mission")

func New(c Config) (*Loop, error) {
	if c.DB == nil || c.Ledger == nil || c.Provider == nil {
		return nil, errors.New("agent: DB, Ledger and Provider are required")
	}
	if c.Model == "" || c.MaxTokens <= 0 || c.MaxTurns <= 0 || c.Wall <= 0 {
		return nil, errors.New("agent: model, max_tokens, max_turns and wall come from the grant and are required")
	}
	if c.MaxServerToolUses < 0 {
		return nil, errors.New("agent: max server-tool uses cannot be negative")
	}
	if err := c.Prices.Validate(); err != nil {
		return nil, err
	}
	if c.BillingMode == "" {
		c.BillingMode = BillingAPIKey
	}
	if c.BillingMode != BillingAPIKey {
		// goodqa#15087: a subscription is used only through the official
		// Claude Code binary (provider claude-code); this loop runs on an
		// API key only.
		return nil, ErrSubscriptionBilling
	}
	if c.MaxResultBytes <= 0 {
		c.MaxResultBytes = 64 << 10
	}
	if c.Redact == nil {
		return nil, errors.New("agent: a Redactor is required (D9: redact at tool-result creation)")
	}
	l := &Loop{c: c, tools: map[string]Tool{}}
	for _, t := range c.Tools {
		l.tools[t.Name()] = t
	}
	return l, nil
}

// configSHA pins everything that shapes the request prefix or the safety
// envelope. A resume under a different model, system prompt, tool set, price
// table or extra parameters is refused: replayed signed thinking would be
// bound to a prefix that no longer exists.
func (l *Loop) configSHA() string {
	c := l.c
	h := sha256.New()
	w := func(s string) { fmt.Fprintf(h, "%d:%s|", len(s), s) }
	w(c.Model)
	w(strconv.Itoa(c.MaxTokens))
	if c.MaxServerToolUses != 0 {
		// Only a non-zero maximum enters the hash, so runs started before
		// this field existed keep their config_sha and can still resume.
		w("max_server_tool_uses=" + strconv.FormatInt(c.MaxServerToolUses, 10))
	}
	w(c.System)
	w(c.Prompt)
	keys := make([]string, 0, len(c.Extra))
	for k := range c.Extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		w(k)
		w(string(c.Extra[k]))
	}
	for _, t := range c.Tools {
		w(string(t.Def()))
		w(strconv.FormatBool(t.SideEffecting()))
	}
	w(c.Pin)
	pj, _ := json.Marshal(c.Prices)
	w(string(pj))
	return hex.EncodeToString(h.Sum(nil))
}

func (l *Loop) crash(point string) {
	if l.c.Crash != nil {
		l.c.Crash(point)
	}
}

func (l *Loop) finish(ctx context.Context, kind, note string, turns int) (Outcome, error) {
	if err := l.c.DB.Finish(ctx, l.c.Attempt, l.c.Fence, kind, note); err != nil {
		return Outcome{}, err
	}
	return Outcome{Kind: kind, Note: note, Turns: turns}, nil
}

func contentBlocks(b []byte) ([]json.RawMessage, error) {
	var out []json.RawMessage
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("stored content is not a block array: %w", err)
	}
	return out, nil
}

// Run drives the attempt until it ends, parks or is stopped. It is safe to
// call again after a crash: it resumes from the checkpointed history and counts
// that new process life against the restart cap.
func (l *Loop) Run(ctx context.Context) (Outcome, error) {
	return l.run(ctx, true)
}

// Resume continues the same live process after a durable-review wait. It
// validates the existing run without consuming another crash restart.
func (l *Loop) Resume(ctx context.Context) (Outcome, error) {
	return l.run(ctx, false)
}

func (l *Loop) run(ctx context.Context, start bool) (Outcome, error) {
	c := l.c
	unlock, err := c.DB.Lock(ctx, c.Attempt)
	if err != nil {
		return Outcome{}, err
	}
	defer unlock()
	var starts int
	if start {
		starts, err = c.DB.StartRun(ctx, c.Attempt, c.Fence, c.BillingMode, l.configSHA(), c.RestartCap)
	} else {
		starts, err = c.DB.ResumeRun(ctx, c.Attempt, c.Fence, l.configSHA())
	}
	if errors.Is(err, ErrRestartCap) {
		return l.finish(ctx, OutcomeParked, "restart cap: "+err.Error(), 0)
	}
	if err != nil {
		return Outcome{}, err
	}
	l.starts = starts
	run, err := c.DB.Run(ctx, c.Attempt)
	if err != nil {
		return Outcome{}, err
	}
	if run.Outcome != "" {
		return Outcome{Kind: run.Outcome, Note: run.OutcomeNote}, nil
	}
	if _, err := c.Ledger.Reap(ctx); err != nil {
		return Outcome{}, fmt.Errorf("reaping holds of a previous life: %w", err)
	}
	started := time.Now().Add(-run.Elapsed)

	msgs, err := c.DB.Messages(ctx, c.Attempt)
	if err != nil {
		return Outcome{}, err
	}
	if len(msgs) == 0 {
		blk, _ := json.Marshal([]json.RawMessage{c.Provider.UserTextBlock(c.Prompt)})
		if err := c.DB.AppendUser(ctx, c.Attempt, c.Fence, 0, blk); err != nil {
			return Outcome{}, err
		}
	}
	for {
		if err := ctx.Err(); err != nil {
			return Outcome{}, err
		}
		msgs, err = c.DB.Messages(ctx, c.Attempt)
		if err != nil {
			return Outcome{}, err
		}
		calls, err := c.DB.ToolCalls(ctx, c.Attempt)
		if err != nil {
			return Outcome{}, err
		}
		last := msgs[len(msgs)-1]
		turns := 0
		for _, m := range msgs {
			if m.Role == "assistant" {
				turns++
			}
		}
		if last.Role == "assistant" {
			var mine []ToolCall
			for _, tc := range calls {
				if tc.MsgIdx == last.Idx {
					mine = append(mine, tc)
				}
			}
			if len(mine) > 0 {
				if err := l.resolveTools(ctx, last, mine); err != nil {
					var paused *reviewPauseError
					if errors.As(err, &paused) {
						// No terminal agent_runs outcome: the same PENDING tool call
						// resumes after its durable approval arrives.
						pause := paused.pause
						return Outcome{Kind: OutcomeApprovalRequired, Note: paused.result, Turns: turns, Review: &pause}, nil
					}
					return Outcome{}, err
				}
				continue
			}
			// An assistant message with no tool calls is terminal: classify
			// why (this also covers a crash between the commit and Finish).
			// Mode C turns a context overflow into one compact-and-retry.
			if c.Compaction.Mode != CompactClient || classifyName(last.StopReason) != StopContext {
				return l.terminalFor(ctx, last, turns)
			}
			comps, err := c.DB.Compactions(ctx, c.Attempt)
			if err != nil {
				return Outcome{}, err
			}
			if n := len(comps); n == 0 || comps[n-1].Idx <= last.Idx {
				out, done, err := l.compactClient(ctx, msgs, comps, turns)
				if err != nil {
					return Outcome{}, err
				}
				if done {
					return out, nil
				}
				continue
			}
			// The overflow was compacted: send the retry below.
		}
		if turns >= c.MaxTurns {
			return l.finish(ctx, OutcomeMaxTurns, strconv.Itoa(turns)+" turns", turns)
		}
		if time.Since(started) >= c.Wall {
			return l.finish(ctx, OutcomeWallExhausted, "wall clock "+c.Wall.String(), turns)
		}
		if c.MaxTotalTokens > 0 {
			used, err := c.DB.TokensUsed(ctx, c.Attempt)
			if err != nil {
				return Outcome{}, err
			}
			if used >= c.MaxTotalTokens {
				return l.finish(ctx, OutcomeBudgetExhausted, "token budget", turns)
			}
		}
		out, done, err := l.turn(ctx, msgs, turns)
		if err != nil {
			return Outcome{}, err
		}
		if done {
			return out, nil
		}
	}
}

func (l *Loop) terminalFor(ctx context.Context, last Message, turns int) (Outcome, error) {
	switch classifyName(last.StopReason) {
	case StopEnd:
		return l.finish(ctx, OutcomeDone, "end_turn", turns)
	case StopMaxTokens:
		return l.finish(ctx, OutcomeParked, "response hit max_tokens before finishing; raise the grant max_tokens or narrow the task", turns)
	case StopRefusal:
		return l.finish(ctx, OutcomeParked, "the model refused the request (stop_reason refusal)", turns)
	case StopContext:
		return l.finish(ctx, OutcomeParked, "context window exceeded", turns)
	case StopPause:
		return l.finish(ctx, OutcomeParked, "pause_turn is not supported in slice 1", turns)
	default:
		return l.finish(ctx, OutcomeParked, "unknown stop_reason "+strconv.Quote(last.StopReason)+": parked, never treated as a normal end", turns)
	}
}

func classifyName(s string) Stop {
	switch s {
	case "end_turn", "stop_sequence":
		return StopEnd
	case "max_tokens":
		return StopMaxTokens
	case "refusal":
		return StopRefusal
	case "model_context_window_exceeded":
		return StopContext
	case "pause_turn":
		return StopPause
	case "tool_use":
		return StopTools
	}
	return StopUnknown
}

func (l *Loop) holdKey(idx, n int) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s/%d/%d/%d/%d", l.c.Attempt, l.c.Fence, l.starts, idx, n)))
	return "ah-" + hex.EncodeToString(h[:20])
}

// charge decides what an attempt cost. A request that got no response at all,
// or an HTTP error, ran no model; a stream that began and did not complete
// cost an unknown amount, so the held worst case stands.
func (l *Loop) charge(r AttemptRecord, held int64) int64 {
	switch r.Outcome {
	case "ok":
		if r.Usage != nil {
			return l.c.Prices.Cost(*r.Usage)
		}
		return held
	case "http_error":
		return 0
	case "transport":
		// The body was written but no response arrived (reset, deadline): the
		// provider may have generated and billed. Only a failure before the
		// write (dial, TLS) is free.
		if r.MaybeBilled {
			return held
		}
		return 0
	default:
		return held
	}
}

func (l *Loop) turn(ctx context.Context, msgs []Message, turns int) (Outcome, bool, error) {
	c := l.c
	req := Request{Model: c.Model, MaxTokens: c.MaxTokens, System: c.System, Extra: c.Extra}
	for _, t := range c.Tools {
		req.Tools = append(req.Tools, t.Def())
	}
	var comps []Compaction
	if c.Compaction.Mode == CompactClient {
		var err error
		if comps, err = c.DB.Compactions(ctx, c.Attempt); err != nil {
			return Outcome{}, false, err
		}
	}
	rm, err := l.requestMessages(msgs, comps)
	if err != nil {
		return Outcome{}, false, err
	}
	req.Messages = rm
	// The estimated-size trigger never fires right after a compaction (the
	// kept history may legitimately exceed a small trigger).
	recent := len(comps) > 0 && comps[len(comps)-1].Idx >= len(msgs)-4
	if c.Compaction.Mode == CompactClient && c.Compaction.TriggerTokens > 0 && !recent &&
		estTokens(requestBytes(c, rm, req.Tools)) > c.Compaction.TriggerTokens {
		return l.compactClient(ctx, msgs, comps, turns)
	}
	idx := len(msgs)
	l.crash("before_hold")
	res, spent, budgetViolation, err := l.heldSend(ctx, req, idx, func(bodyBytes int) int64 {
		amt := c.Prices.WorstCase(bodyBytes, c.MaxTokens, c.MaxServerToolUses)
		if c.Compaction.Mode == CompactServer {
			// The compaction pass: one more worst case, bounded by the grant's
			// compaction_output_cap (goodqa#15361 FN-5).
			amt += c.Prices.WorstCase(bodyBytes, c.Compaction.OutputCap, 0)
		}
		return amt
	})
	if errors.Is(err, ErrContextOverflow) && c.Compaction.Mode == CompactClient {
		return l.compactClient(ctx, msgs, comps, turns)
	}
	if err != nil {
		return l.sendFailure(ctx, err, turns)
	}
	if budgetViolation != "" {
		o, ferr := l.finish(ctx, OutcomeBudgetExhausted, budgetViolation, turns)
		return o, true, ferr
	}
	blk, _ := json.Marshal(res.Content)
	usage, _ := json.Marshal(res.Usage)
	attemptsJSON, _ := json.Marshal(res.Attempts)
	t := AssistantTurn{
		Content: blk, RequestSHA: res.RequestSHA, RequestIDs: res.RequestIDs, StopReason: res.StopReason,
		Model: res.Model, UsageJSON: usage, CostMicros: spent, CostLabel: costLabel(c.BillingMode), AttemptsJSON: attemptsJSON,
	}
	if res.Stop == StopTools {
		if len(res.ToolUses) == 0 {
			t.Calls = nil
		}
		for _, tu := range res.ToolUses {
			se := true
			if tool, ok := l.tools[tu.Name]; ok {
				se = tool.SideEffecting()
			}
			t.Calls = append(t.Calls, ToolCall{ID: tu.ID, Name: tu.Name, Input: tu.Input, SideEffecting: se})
		}
	}
	if err := c.DB.CommitAssistant(ctx, c.Attempt, c.Fence, idx, t); err != nil {
		return Outcome{}, false, err
	}
	l.crash("after_tool_use")
	if res.Stop == StopTools && len(t.Calls) == 0 {
		o, ferr := l.finish(ctx, OutcomeParked, "stop_reason tool_use without a tool_use block", turns+1)
		return o, true, ferr
	}
	return Outcome{}, false, nil
}

func costLabel(mode string) string {
	if mode == BillingSubscription {
		return LabelNotional
	}
	return LabelAPIEstimate
}

const interruptedText = "interrupted: the runner stopped while this tool was running, so whether it took effect is unknown. Check the current state before doing it again."

type reviewPauseError struct {
	result string
	pause  ReviewPause
}

func (e *reviewPauseError) Error() string { return "agent: durable review is unresolved" }

// resolveTools brings every tool call of the assistant message to a terminal
// state, then commits the tool_result message. Nothing runs before the
// assistant message is committed. Ordinary side-effecting calls left EXECUTING
// are interrupted; reviewed calls stay PENDING and use their durable outcome.
func (l *Loop) resolveTools(ctx context.Context, last Message, calls []ToolCall) error {
	c := l.c
	results := map[string]ToolCall{}
	for _, tc := range calls {
		cur := tc
		switch tc.State {
		case "DONE", "INTERRUPTED":
		case "EXECUTING":
			if tc.SideEffecting {
				note := ""
				if n, ok := l.tools[tc.Name].(InterruptNoter); ok {
					note = n.InterruptedNote(tc.ID, tc.Input)
				}
				text := c.Redact.Redact(interruptedText + note)
				if err := c.DB.FinishTool(ctx, c.Attempt, c.Fence, tc.ID, "INTERRUPTED", []byte(text), true); err != nil {
					return err
				}
				cur.State, cur.Result, cur.IsError = "INTERRUPTED", []byte(text), true
				break
			}
			fallthrough
		default: // PENDING, or a read-only EXECUTING call that is safe to repeat
			tool := l.tools[tc.Name]
			reviewed, isReviewed := tool.(DurableReviewTool)
			if tc.State == "PENDING" && !isReviewed {
				if err := c.DB.MarkExecuting(ctx, c.Attempt, c.Fence, tc.ID); err != nil {
					return err
				}
				l.crash("after_mark_executing")
			}
			if isReviewed {
				result, err := l.executeReviewed(ctx, reviewed, tc)
				if err != nil {
					return err
				}
				if result.Pause != nil {
					return &reviewPauseError{result: result.Output, pause: *result.Pause}
				}
				l.crash("before_finish_tool")
				if result.GuardDenied {
					if err := c.DB.MarkExecuting(ctx, c.Attempt, c.Fence, tc.ID); err != nil {
						return err
					}
					if err := c.DB.FinishTool(ctx, c.Attempt, c.Fence, tc.ID, "DONE", []byte(result.Output), result.IsError); err != nil {
						return err
					}
				} else if err := c.DB.FinishReviewedTool(ctx, c.Attempt, c.Fence, tc.ID, []byte(result.Output), result.IsError); err != nil {
					return err
				}
				cur.State, cur.Result, cur.IsError = "DONE", []byte(result.Output), result.IsError
				break
			}
			out, isErr := l.execute(ctx, tc)
			l.crash("before_finish_tool")
			if err := c.DB.FinishTool(ctx, c.Attempt, c.Fence, tc.ID, "DONE", []byte(out), isErr); err != nil {
				return err
			}
			cur.State, cur.Result, cur.IsError = "DONE", []byte(out), isErr
		}
		results[tc.ID] = cur
	}
	l.crash("after_tool_result")
	assistant, err := contentBlocks(mustContent(ctx, c.DB, c.Attempt, last.Idx))
	if err != nil {
		return err
	}
	var blocks []json.RawMessage
	for _, b := range assistant {
		var h struct{ Type, ID string }
		if json.Unmarshal(b, &h) != nil || h.Type != "tool_use" {
			continue
		}
		r, ok := results[h.ID]
		if !ok {
			return fmt.Errorf("tool_use %s has no recorded call", h.ID)
		}
		blocks = append(blocks, c.Provider.ToolResultBlock(h.ID, string(r.Result), r.IsError))
	}
	body, _ := json.Marshal(blocks)
	return c.DB.AppendToolResults(ctx, c.Attempt, c.Fence, last.Idx+1, body)
}

func mustContent(ctx context.Context, db *DB, attempt domain.AttemptID, idx int) []byte {
	msgs, _ := db.Messages(ctx, attempt)
	for _, m := range msgs {
		if m.Idx == idx {
			return m.Content
		}
	}
	return nil
}

// execute runs one tool behind the guard and redacts its output at creation.
func (l *Loop) execute(ctx context.Context, tc ToolCall) (string, bool) {
	c := l.c
	tool, ok := l.tools[tc.Name]
	if !ok {
		return "unknown tool " + strconv.Quote(tc.Name), true
	}
	if c.Guard != nil {
		if d := c.Guard.Check(ctx, tc.Name, tc.Input); !d.Allow {
			return c.Redact.Redact("refused by policy: " + d.Reason), true
		}
	}
	toolCtx := WithToolCallContext(ctx, ToolCallContext{ID: tc.ID, MessageIndex: tc.MsgIdx})
	out, isErr := tool.Run(toolCtx, tc.Input)
	out = c.Redact.Redact(out)
	if len(out) > c.MaxResultBytes {
		out = out[:c.MaxResultBytes] + "\n[output truncated]"
	}
	return out, isErr
}

// executeReviewed passes trusted guard identity out-of-band and preserves the
// reviewed tool's typed control outcome. Stdout can never manufacture a pause.
func (l *Loop) executeReviewed(ctx context.Context, tool DurableReviewTool, tc ToolCall) (ReviewRunResult, error) {
	toolCtx := WithToolCallContext(ctx, ToolCallContext{ID: tc.ID, MessageIndex: tc.MsgIdx, Guard: l.c.Guard})
	result, err := tool.RunReviewed(toolCtx, tc.Input)
	if err != nil {
		return ReviewRunResult{}, err
	}
	result.Output = l.c.Redact.Redact(result.Output)
	if len(result.Output) > l.c.MaxResultBytes {
		result.Output = result.Output[:l.c.MaxResultBytes] + "\n[output truncated]"
	}
	return result, nil
}
