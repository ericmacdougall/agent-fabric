package agent

// Context compaction (goodqa#15361 N3).
//
// Mode S (server): the provider compacts inside the request. The loop only
// stores the compaction block verbatim, omits the messages before it from the
// next request, holds for the extra pass and charges usage.iterations.
//
// Mode C (client): when the context fills, the loop summarises the older
// history with one extra request, records it as an append-only
// agent_compactions row and sends the summary plus the newest messages. The
// row is a recorded effect: a resume reads it and never regenerates it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"fabric/internal/store"
)

// Compaction modes.
const (
	CompactServer = "server"
	CompactClient = "client"
)

// CompactionConfig comes from the validated grant.
type CompactionConfig struct {
	Mode string
	// TriggerTokens: mode C fires when the estimated input passes it (0: only
	// on overflow). Mode S sends it as the API trigger (0: the API default).
	TriggerTokens int
	// OutputCap: mode S, the bound on the compaction pass output used in the
	// hold; mode C, max_tokens of the summary request.
	OutputCap int
	// WindowTokens is the model's context window (mode C cut rules).
	WindowTokens int
	// KeepPrefix (mode S) keeps the messages before the compaction block in
	// the request instead of omitting them.
	KeepPrefix bool
	// StripThinking (mode C) removes thinking blocks from kept assistant
	// messages except the last one (the N3-THINKING amendment, off by default).
	StripThinking bool
}

// ServerInstructions is sent with server compaction: the docs warn that with
// tools defined the summarising step may call a tool and return a null summary.
const ServerInstructions = "Summarize the transcript inside <summary></summary> tags. Include relevant information in the summary for continuing the task in the next context window. Do not call any tools while writing this summary; respond with text only."

const summarySystem = `You write a handoff summary of an agent's work so far, so another instance can continue it. Use exactly these headings: Goal, Constraints, Progress, Decisions, Next steps, Critical context, Files read, Files modified. Be specific: names, paths, commands, results. Do not invent facts. Do not call tools. Respond with the summary only.`

const bytesPerToken = 3.5

func estTokens(bytes int) int { return int(float64(bytes) / bytesPerToken) }

// ---- request construction ----

type reqMsg struct {
	abs    int // index in agent_messages; -1 for the synthetic summary
	role   string
	blocks []json.RawMessage
}

func blockType(b json.RawMessage) string {
	var h struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(b, &h)
	return h.Type
}

// compactionContent returns a compaction block's summary and whether it holds one.
func compactionContent(b json.RawMessage) (string, bool) {
	var h struct {
		Type    string  `json:"type"`
		Content *string `json:"content"`
	}
	if json.Unmarshal(b, &h) != nil || h.Type != "compaction" || h.Content == nil {
		return "", false
	}
	return *h.Content, true
}

// requestMessages is the history as it is sent: what follows the latest
// compaction (server block, or client row), minus overflow turns.
func (l *Loop) requestMessages(msgs []Message, comps []Compaction) ([]Msg, error) {
	cfg := l.c.Compaction
	var all []reqMsg
	start := 0
	if cfg.Mode == CompactClient && len(comps) > 0 {
		last := comps[len(comps)-1]
		sb, _ := json.Marshal(map[string]string{"type": "text", "text": "Summary of the earlier conversation (older messages were compacted):\n\n" + last.Summary})
		all = append(all, reqMsg{abs: -1, role: "user", blocks: []json.RawMessage{sb}})
		start = last.FirstKeptIdx
	}
	for _, m := range msgs {
		if m.Idx < start {
			continue
		}
		// A response that overflowed the window carries no work to resend.
		if cfg.Mode == CompactClient && m.Role == "assistant" && classifyName(m.StopReason) == StopContext {
			continue
		}
		blocks, err := contentBlocks(m.Content)
		if err != nil {
			return nil, err
		}
		all = append(all, reqMsg{abs: m.Idx, role: m.Role, blocks: blocks})
	}
	if cfg.Mode == CompactServer && !cfg.KeepPrefix {
		// The API drops everything before a compaction block, so the client
		// may omit it; only a block that holds a summary counts.
		cut := -1
		for i, m := range all {
			if m.role != "assistant" {
				continue
			}
			for _, b := range m.blocks {
				if _, ok := compactionContent(b); ok {
					cut = i
				}
			}
		}
		if cut > 0 {
			all = all[cut:]
		}
	}
	if cfg.Mode == CompactClient && cfg.StripThinking {
		lastAsst := -1
		for i, m := range all {
			if m.role == "assistant" {
				lastAsst = i
			}
		}
		var kept []reqMsg
		for i, m := range all {
			if m.role == "assistant" && i < lastAsst {
				var bs []json.RawMessage
				for _, b := range m.blocks {
					if t := blockType(b); t != "thinking" && t != "redacted_thinking" {
						bs = append(bs, b)
					}
				}
				if len(bs) == 0 {
					continue
				}
				m.blocks = bs
			}
			kept = append(kept, m)
		}
		all = kept
	}
	out := make([]Msg, 0, len(all))
	for _, m := range all {
		out = append(out, Msg{Role: m.role, Content: m.blocks})
	}
	return out, nil
}

func msgBytes(blocks []json.RawMessage) int {
	n := 0
	for _, b := range blocks {
		n += len(b)
	}
	return n
}

func requestBytes(c Config, msgs []Msg, tools []json.RawMessage) int {
	n := len(c.System)
	for _, t := range tools {
		n += len(t)
	}
	for _, m := range msgs {
		n += msgBytes(m.Content)
	}
	return n
}

// ---- mode C ----

const (
	keepFraction    = 0.5
	summaryFraction = 0.6
)

// errNoCut parks the attempt.
var errNoCut = errors.New("context window exceeded: no valid cut")

// planCut picks firstKept: the OLDEST assistant message whose suffix fits in
// half the window. The suffix therefore starts at an assistant message, so no
// tool_result is orphaned. It returns the messages before the cut as the
// summary input (the earlier summary, when there is one, comes first).
func (l *Loop) planCut(msgs []Message, comps []Compaction) (firstKept int, prefix []reqMsg, err error) {
	cfg := l.c.Compaction
	live, rerr := l.liveMessages(msgs, comps)
	if rerr != nil {
		return 0, nil, rerr
	}
	budget := int(float64(cfg.WindowTokens) * bytesPerToken * keepFraction)
	suffix := make([]int, len(live)+1) // suffix[i] = bytes of live[i:]
	for i := len(live) - 1; i >= 0; i-- {
		suffix[i] = suffix[i+1] + msgBytes(live[i].blocks)
	}
	first := 0
	if len(live) > 0 && live[0].abs < 0 {
		first = 1 // the synthetic summary is never a cut point
	}
	for i := first + 1; i < len(live); i++ { // at least one real message precedes the cut
		if live[i].role == "assistant" && suffix[i] <= budget {
			return live[i].abs, live[:i], nil
		}
	}
	return 0, nil, errNoCut
}

func (l *Loop) liveMessages(msgs []Message, comps []Compaction) ([]reqMsg, error) {
	var all []reqMsg
	start := 0
	if len(comps) > 0 {
		last := comps[len(comps)-1]
		sb, _ := json.Marshal(map[string]string{"type": "text", "text": last.Summary})
		all = append(all, reqMsg{abs: -1, role: "user", blocks: []json.RawMessage{sb}})
		start = last.FirstKeptIdx
	}
	for _, m := range msgs {
		if m.Idx < start || (m.Role == "assistant" && classifyName(m.StopReason) == StopContext) {
			continue
		}
		blocks, err := contentBlocks(m.Content)
		if err != nil {
			return nil, err
		}
		all = append(all, reqMsg{abs: m.Idx, role: m.Role, blocks: blocks})
	}
	return all, nil
}

const toolResultClip = 4000

func renderBlock(b json.RawMessage) string {
	var h struct {
		Type    string          `json:"type"`
		Text    string          `json:"text"`
		Name    string          `json:"name"`
		Input   json.RawMessage `json:"input"`
		Content json.RawMessage `json:"content"`
		IsError bool            `json:"is_error"`
	}
	if json.Unmarshal(b, &h) != nil {
		return ""
	}
	switch h.Type {
	case "text":
		return h.Text
	case "tool_use":
		return "[tool call " + h.Name + " " + string(h.Input) + "]"
	case "tool_result":
		var s string
		if json.Unmarshal(h.Content, &s) != nil {
			s = string(h.Content)
		}
		if len(s) > toolResultClip {
			s = s[:toolResultClip] + " [clipped]"
		}
		if h.IsError {
			return "[tool error] " + s
		}
		return "[tool result] " + s
	case "compaction":
		var s string
		_ = json.Unmarshal(h.Content, &s)
		return s
	}
	return "" // thinking and unknown blocks are not summarised
}

// summaryInput renders the prefix as one user message, cutting from the oldest
// side to fit in 60% of the window and marking the cut.
func (l *Loop) summaryInput(prefix []reqMsg) string {
	limit := int(float64(l.c.Compaction.WindowTokens) * bytesPerToken * summaryFraction)
	var parts []string
	for _, m := range prefix {
		var sb strings.Builder
		who := m.role
		if m.abs < 0 {
			who = "earlier summary"
		}
		sb.WriteString("[" + who + "]\n")
		for _, b := range m.blocks {
			if t := renderBlock(b); t != "" {
				sb.WriteString(t)
				sb.WriteByte('\n')
			}
		}
		parts = append(parts, sb.String())
	}
	total := 0
	for _, p := range parts {
		total += len(p)
	}
	omitted := false
	for len(parts) > 1 && total > limit {
		total -= len(parts[0])
		parts = parts[1:]
		omitted = true
	}
	text := strings.Join(parts, "\n")
	if len(text) > limit {
		text = text[len(text)-limit:]
		omitted = true
	}
	if omitted {
		text = "[earlier history omitted]\n" + text
	}
	return "Conversation to summarise:\n\n" + text
}

// compactClient runs one compaction. done reports a terminal outcome (a park
// or budget stop); otherwise the loop goes on and the next request is built
// from the committed row.
func (l *Loop) compactClient(ctx context.Context, msgs []Message, comps []Compaction, turns int) (Outcome, bool, error) {
	c := l.c
	position := len(msgs)
	if n := len(comps); n > 0 && comps[n-1].Idx >= position-1 {
		o, err := l.finish(ctx, OutcomeParked, "context window exceeded after compaction", turns)
		return o, true, err
	}
	firstKept, prefix, err := l.planCut(msgs, comps)
	if errors.Is(err, errNoCut) {
		o, ferr := l.finish(ctx, OutcomeParked, errNoCut.Error(), turns)
		return o, true, ferr
	}
	if err != nil {
		return Outcome{}, false, err
	}
	in, _ := json.Marshal(map[string]string{"type": "text", "text": l.summaryInput(prefix)})
	capTokens := c.Compaction.OutputCap
	if capTokens <= 0 {
		capTokens = 4096
	}
	req := Request{Model: c.Model, MaxTokens: capTokens, System: summarySystem,
		Messages: []Msg{{Role: "user", Content: []json.RawMessage{in}}}}
	res, spent, violation, err := l.heldSend(ctx, req, 1<<30+len(comps)<<20+position, func(body int) int64 {
		return c.Prices.WorstCase(body, capTokens, 0)
	})
	if err != nil {
		return l.sendFailure(ctx, err, turns)
	}
	if violation != "" {
		o, ferr := l.finish(ctx, OutcomeBudgetExhausted, violation, turns)
		return o, true, ferr
	}
	var sb strings.Builder
	for _, b := range res.Content {
		if blockType(b) == "text" {
			var t struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(b, &t)
			sb.WriteString(t.Text)
		}
	}
	summary := strings.TrimSpace(sb.String())
	if summary == "" || res.Stop != StopEnd {
		o, ferr := l.finish(ctx, OutcomeParked, "compaction produced no usable summary (stop_reason "+res.StopReason+")", turns)
		return o, true, ferr
	}
	l.crash("after_summary_send")
	if err := c.DB.CommitCompaction(ctx, c.Attempt, c.Fence, Compaction{Idx: position, FirstKeptIdx: firstKept, Summary: summary, RequestSHA: res.RequestSHA, CostMicros: spent}); err != nil {
		return Outcome{}, false, err
	}
	l.crash("after_compaction_commit")
	return Outcome{}, false, nil
}

// heldSend is one provider request under a ledger hold: the hold is taken
// before the first byte is sent, every attempt is settled, and a settled cost
// above the hold comes back as a violation (never silently overrun).
func (l *Loop) heldSend(ctx context.Context, req Request, keyIdx int, hold func(bodyBytes int) int64) (*Result, int64, string, error) {
	c := l.c
	held := map[int]int64{}
	var spent int64
	var violation string
	reserved := map[int][]store.Reservation{}
	l.settleE = nil
	hooks := Hooks{
		BeforeAttempt: func(n, bodyBytes int) error {
			amt := hold(bodyBytes)
			rs, err := c.Ledger.Hold(ctx, l.holdKey(keyIdx, n), amt)
			if err != nil {
				return err
			}
			held[n] = amt
			reserved[n] = rs
			l.crash("after_hold")
			return nil
		},
		AfterAttempt: func(r AttemptRecord) {
			actual := l.charge(r, held[r.N])
			spent += actual
			if r.Outcome == "ok" {
				// The hold is the worst case; a larger actual means the bound did
				// not hold. Settle the full actual below, then refuse the turn.
				if actual > held[r.N] {
					violation = fmt.Sprintf("actual cost %d exceeded held reservation %d", actual, held[r.N])
				}
				if r.Usage != nil && r.Usage.ServerToolUses > c.MaxServerToolUses {
					if violation != "" {
						violation += "; "
					}
					violation += fmt.Sprintf("server-tool uses %d exceeded configured maximum %d", r.Usage.ServerToolUses, c.MaxServerToolUses)
				}
			}
			if err := c.Ledger.Settle(ctx, reserved[r.N], actual); err != nil && l.settleE == nil {
				l.settleE = err
			}
		},
		OnEvent: func(n int) { l.crash("mid_stream") },
	}
	res, err := c.Provider.Send(ctx, req, hooks)
	if l.settleE != nil {
		return nil, spent, violation, l.settleE
	}
	return res, spent, violation, err
}

// sendFailure maps a failed Send to the loop's terminal outcomes.
func (l *Loop) sendFailure(ctx context.Context, err error, turns int) (Outcome, bool, error) {
	if l.settleE != nil {
		return Outcome{}, false, l.settleE
	}
	var eb *ErrBudget
	if errors.As(err, &eb) {
		o, ferr := l.finish(ctx, OutcomeBudgetExhausted, eb.Error(), turns)
		return o, true, ferr
	}
	var pe *ParkError
	if errors.As(err, &pe) {
		o, ferr := l.finish(ctx, OutcomeParked, pe.Reason, turns)
		return o, true, ferr
	}
	return Outcome{}, false, err
}
