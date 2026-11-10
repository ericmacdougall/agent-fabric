package codex

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"fabric/internal/harness"
)

// Phases of the handshake.
const (
	phInit   = iota // waiting for the initialize reply
	phThread        // waiting for the thread/start or thread/resume reply
	phReady         // turns may be dispatched
)

// turn is the one turn in flight. Its identity is established only by the
// turn/start reply (turnID == "" until then); frames and completions that
// arrive earlier are held, bounded, and replayed or dropped on binding.
type turn struct {
	turnID     string
	answered   bool // a message or tool call has been reported for it
	interrupt  bool // an interrupt was requested
	frames     []frame
	frameBytes int
	dropped    bool
	pending    []completion
	pendBytes  int
	usage      *harness.Usage
}

type frame struct {
	method string
	turn   string
	raw    json.RawMessage
}

type completion struct {
	turn, status, message string
}

type packet struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// readLoop turns stdout into lineEv, enforcing the line and total bounds.
func (s *session) readLoop() {
	defer s.wg.Done()
	defer close(s.readerDone)
	lr := newLineReader(s.p.stdout, s.maxLine)
	var total int64
	for {
		l, err := lr.next()
		if err != nil {
			s.post(exitEv{code: -1})
			return
		}
		total += int64(l.size)
		if total > s.maxTotal {
			s.post(limitEv{})
			return
		}
		if !s.post(lineEv{l}) {
			return
		}
	}
}

// observeLoop detects leader exit without reaping it, including when an owned
// child holds stdout open. Protocol lines get a bounded drain opportunity.
func (s *session) observeLoop() {
	defer s.wg.Done()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-s.quit:
			return
		case <-tick.C:
		}
		exited, err := s.p.leaderExited()
		if !exited && err == nil {
			continue
		}
		select {
		case <-s.readerDone:
		case <-s.quit:
			return
		case <-time.After(s.grace):
		}
		s.post(exitEv{code: -1, err: err})
		return
	}
}

// run is the actor. It is the only goroutine that touches protocol state and
// the only one that emits events.
func (s *session) run() {
	defer close(s.done)
	if err := s.request("initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "fabric-codex-adapter", "version": "1"},
		"capabilities": map[string]any{"experimentalApi": true},
	}, "initialize"); err != nil {
		s.fail("cannot talk to codex: " + err.Error())
		return
	}
	for !s.finished {
		select {
		case ev := <-s.in:
			s.handle(ev)
		case <-s.timerC:
			s.timerC = nil
			s.fail(fmt.Sprintf("codex did not answer %s within %s", s.timerKind, s.reqTimeout))
		}
		if !s.finished && s.em.Err() != nil {
			// A run whose events cannot be recorded must not keep acting.
			s.fail("event sink failed: " + s.em.Err().Error())
		}
	}
}

func (s *session) handle(ev any) {
	switch e := ev.(type) {
	case lineEv:
		s.onLine(e.l)
	case limitEv:
		s.fail(fmt.Sprintf("codex output exceeded %d bytes", s.maxTotal))
	case exitEv:
		s.finish(harness.Final{Outcome: harness.OutcomeFailed, Reason: "codex exited without a result: " + exitText(e.code, e.err), ExitCode: e.code})
	case inputCmd:
		switch {
		case len(s.queue) >= MaxQueuedInputs:
			e.reply <- errors.New("codex: input queue full")
		default:
			s.queue = append(s.queue, e.msg)
			e.reply <- nil
			s.dispatch()
		}
	case interruptCmd:
		s.interrupt()
		e.reply <- nil
	case stopCmd:
		e.reply <- nil
		s.finish(harness.Final{Outcome: harness.OutcomeStopped, ExitCode: -1})
	}
}

// ---- outbound ----

func (s *session) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.p.write(append(b, '\n'), writeDeadline)
}

// request sends a request whose reply is routed by kind. initialize, thread
// and turn replies are time-bounded.
func (s *session) request(method string, params any, kind string) error {
	s.nextReq++
	id := s.nextReq
	s.pend[id] = kind
	if kind != "interrupt" {
		s.arm(kind)
	}
	return s.send(map[string]any{"id": id, "method": method, "params": params})
}

func (s *session) arm(kind string) {
	s.disarm()
	s.timerKind = kind
	s.timer = time.NewTimer(s.reqTimeout)
	s.timerC = s.timer.C
}

func (s *session) disarm() {
	if s.timer != nil {
		s.timer.Stop()
	}
	s.timer, s.timerC = nil, nil
}

func (s *session) respond(id json.RawMessage, result any, errMsg string) {
	m := map[string]any{"id": id}
	if errMsg != "" {
		m["error"] = map[string]any{"code": -32601, "message": errMsg}
	} else {
		m["result"] = result
	}
	_ = s.send(m)
}

// dispatch starts the next queued turn when nothing is in flight.
func (s *session) dispatch() {
	if s.finished || s.phase != phReady || s.cur != nil || len(s.queue) == 0 {
		return
	}
	msg := s.queue[0]
	s.queue = s.queue[1:]
	t := &turn{}
	s.cur = t
	_ = s.em.Message(s.ctx, "user", clip(msg, 2*harness.MaxText))
	params := map[string]any{
		"threadId":       s.threadID(),
		"cwd":            s.spec.Dir,
		"input":          []any{map[string]any{"type": "text", "text": msg, "text_elements": []any{}}},
		"approvalPolicy": s.opts.approval,
		"sandboxPolicy":  sandboxPolicy(s.opts.sandbox, s.spec.Dir),
	}
	if s.control != nil {
		delete(params, "sandboxPolicy")
	}
	if s.spec.Model != "" {
		params["model"] = s.spec.Model
	}
	if err := s.request("turn/start", params, "turn"); err != nil {
		s.fail("cannot send turn/start: " + err.Error())
		return
	}
}

func sandboxPolicy(mode, dir string) map[string]any {
	if mode == "read-only" {
		return map[string]any{"type": "readOnly", "networkAccess": false}
	}
	return map[string]any{"type": "workspaceWrite", "writableRoots": []string{dir}, "networkAccess": false}
}

func (s *session) threadID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.thread
}

// interrupt cancels the running turn, once, and keeps the session.
func (s *session) interrupt() {
	t := s.cur
	if s.finished || t == nil || t.interrupt {
		return
	}
	t.interrupt = true
	if t.turnID != "" {
		s.sendInterrupt(t)
	} // otherwise it is sent as soon as the reply binds the turn
}

func (s *session) sendInterrupt(t *turn) {
	if err := s.request("turn/interrupt", map[string]any{"threadId": s.threadID(), "turnId": t.turnID}, "interrupt"); err != nil {
		s.fail("cannot send turn/interrupt: " + err.Error())
	}
}

// ---- inbound ----

func (s *session) onLine(l line) {
	if l.over {
		s.unknown(fmt.Sprintf("protocol line of %d bytes exceeds the %d byte limit", l.size, s.maxLine), "")
		return
	}
	if len(strings.TrimSpace(string(l.data))) == 0 {
		return
	}
	var pk packet
	if err := json.Unmarshal(l.data, &pk); err != nil {
		s.unknown("malformed JSON from harness", string(l.data))
		return
	}
	switch {
	case len(pk.ID) > 0 && pk.Method == "":
		s.onResponse(pk, l.data)
	case len(pk.ID) > 0:
		s.onServerRequest(pk)
	case pk.Method != "":
		s.onNotification(pk)
	default:
		s.unknown("JSON message is not a request, response or notification", string(l.data))
	}
}

func (s *session) onResponse(pk packet, raw []byte) {
	var id uint64
	if json.Unmarshal(pk.ID, &id) != nil {
		s.unknown("response with an id this adapter did not issue", string(raw))
		return
	}
	kind, ok := s.pend[id]
	if !ok {
		s.unknown("response to a request this adapter did not send", string(raw))
		return
	}
	delete(s.pend, id)
	if kind != "interrupt" {
		s.disarm()
	}
	if pk.Error != nil {
		if kind == "interrupt" {
			if s.control != nil {
				s.unknown("turn/interrupt rejected", "")
				return
			}
			s.unknown("turn/interrupt rejected: "+pk.Error.Message, "")
			return
		}
		if s.control != nil {
			s.fail(fmt.Sprintf("codex rejected %s (code %d)", kind, pk.Error.Code))
		} else {
			s.fail(fmt.Sprintf("codex rejected %s: %s", kind, pk.Error.Message))
		}
		return
	}
	switch kind {
	case "initialize":
		if err := s.send(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
			s.fail("cannot send initialized: " + err.Error())
			return
		}
		if s.control != nil {
			if err := s.request("permissionProfile/list", map[string]any{"cwd": s.spec.Dir, "limit": 100}, "controlled_profiles"); err != nil {
				s.fail("codex: profile metadata unavailable")
			}
		} else {
			s.startThread()
		}
	case "controlled_profiles":
		if err := s.controlledProfiles(pk.Result); err != nil {
			s.fail(err.Error())
			return
		}
		if s.control.CatalogOnly {
			s.finish(harness.Final{Outcome: harness.OutcomeStopped, Reason: "controlled profile metadata checked; no thread or model turn", ExitCode: -1})
			return
		}
		s.startThread()
	case "thread":
		if s.control != nil {
			if err := s.control.checkThread(pk.Result, s.spec.Dir); err != nil {
				s.fail(err.Error())
				return
			}
		}
		var r struct {
			Thread struct {
				ID string `json:"id"`
			} `json:"thread"`
		}
		if json.Unmarshal(pk.Result, &r) != nil || r.Thread.ID == "" {
			s.fail("codex thread reply carries no thread id")
			return
		}
		if s.opts.resume != "" && r.Thread.ID != s.opts.resume {
			s.fail("codex resumed a different thread than requested")
			return
		}
		s.mu.Lock()
		s.thread = r.Thread.ID
		s.mu.Unlock()
		s.phase = phReady
		if s.control != nil && s.control.PreflightOnly {
			s.finish(harness.Final{Outcome: harness.OutcomeStopped, Reason: "controlled preflight complete; no model turn", ExitCode: -1})
			return
		}
		s.dispatch()
	case "turn":
		s.bind(pk.Result)
	}
}

// bind establishes the turn identity from the turn/start reply and releases
// what arrived before it.
func (s *session) bind(result json.RawMessage) {
	t := s.cur
	if t == nil || t.turnID != "" {
		s.unknown("turn/start reply with no turn waiting for it", "")
		return
	}
	id := frameTurn(result, true)
	if id == "" {
		s.fail("codex turn/start reply omitted or ambiguously specified the turn")
		return
	}
	t.turnID = id
	s.turnIdx++
	_ = s.em.Turn(s.ctx, s.turnIdx)
	frames := t.frames
	t.frames, t.frameBytes = nil, 0
	for _, f := range frames {
		if f.turn == id {
			s.applyContent(f.method, f.raw)
		} else {
			s.unknown("content frame for another turn", f.method)
		}
	}
	for _, c := range t.pending {
		if c.turn == id {
			t.pending = nil
			s.finishTurn(c.status, c.message)
			return
		}
	}
	t.pending, t.pendBytes = nil, 0
	if t.interrupt {
		s.sendInterrupt(t)
	}
}

func (s *session) onServerRequest(pk packet) {
	switch pk.Method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		var p struct {
			ItemID string `json:"itemId"`
		}
		_ = json.Unmarshal(pk.Params, &p)
		name := "command"
		if strings.Contains(pk.Method, "fileChange") {
			name = "file_change"
		}
		_ = s.em.ToolCall(s.ctx, p.ItemID, name, pk.Params, "denied")
		s.respond(pk.ID, map[string]any{"decision": "decline"}, "")
	default:
		s.unknown("unsupported server request "+pk.Method+"; none was granted", string(pk.Params))
		s.respond(pk.ID, nil, "this client does not support that request; nothing was approved")
	}
}

// benign notifications carry progress that the completed item restates, or
// session chatter with no bearing on the run.
var benign = map[string]bool{
	"turn/started": true, "turn/diff/updated": true, "turn/plan/updated": true,
	"thread/status/changed": true, "thread/name/updated": true, "thread/compacted": true,
	"thread/settings/updated": true, "account/updated": true, "account/rateLimits/updated": true,
	"skills/changed": true, "configWarning": true, "mcpServer/startupStatus/updated": true,
	"item/mcpToolCall/progress": true, "serverRequest/resolved": true,
	"remoteControl/status/changed": true, "thread/tokenUsage/reset": true,
	"thread/readState/changed": true, "thread/queue/changed": true, "thread/section/changed": true,
	"thread/project/updated": true, "thread/attachment/updated": true, "thread/prediction/updated": true,
	"project/changed": true, "app/list/updated": true, "hook/started": true, "hook/completed": true,
	"deprecationNotice": true, "thread/environment/connected": true, "thread/environment/disconnected": true,
	"modelProvider/authRecoveryStarted": true, "modelProvider/authRecoveryCompleted": true,
	"item/commandExecution/terminalInteraction": true,
}

func isBenign(method string) bool {
	return benign[method] || strings.HasSuffix(method, "Delta") || strings.HasSuffix(method, "/delta")
}

func (s *session) onNotification(pk packet) {
	m := pk.Method
	var head struct {
		ThreadID string `json:"threadId"`
		Thread   struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	_ = json.Unmarshal(pk.Params, &head)
	if m == "thread/started" {
		if head.Thread.ID == "" || head.Thread.ID == s.threadID() {
			return
		}
		s.unknown("another thread started (subagents are not tracked)", m)
		return
	}
	if isBenign(m) {
		return
	}
	if m == "warning" || m == "guardianWarning" {
		if head.ThreadID != "" && head.ThreadID != s.threadID() {
			s.unknown("warning for another thread (subagents are not tracked)", m)
			return
		}
		var w struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(pk.Params, &w)
		if s.control != nil {
			_ = s.em.Message(s.ctx, "system", "codex warning")
		} else {
			_ = s.em.Message(s.ctx, "system", "codex warning: "+clip(w.Message, harness.MaxText))
		}
		return
	}
	switch m {
	case "item/started", "item/completed", "thread/tokenUsage/updated", "error", "turn/completed":
	default:
		s.unknown("unrecognised notification "+m, string(pk.Params))
		return
	}
	if head.ThreadID != "" && head.ThreadID != s.threadID() {
		s.unknown("event for another thread (subagents are not tracked)", m)
		return
	}
	isDone := m == "turn/completed"
	id := frameTurn(pk.Params, isDone)
	if id == "" {
		s.unknown(m+" without an unambiguous turn identity", string(pk.Params))
		return
	}
	t := s.cur
	if t == nil {
		s.unknown(m+" with no turn in flight", "")
		return
	}
	if isDone {
		var p struct {
			Turn struct {
				Status string `json:"status"`
				Error  struct {
					Message string `json:"message"`
				} `json:"error"`
			} `json:"turn"`
		}
		if json.Unmarshal(pk.Params, &p) != nil {
			s.unknown("turn/completed does not decode", string(pk.Params))
			return
		}
		c := completion{turn: id, status: p.Turn.Status, message: p.Turn.Error.Message}
		switch {
		case t.turnID == "":
			if len(t.pending) >= maxPendingDone || len(c.turn)+len(c.status)+len(c.message) > maxPendingBytes-t.pendBytes {
				s.fail("codex sent too many completions before the turn was bound")
				return
			}
			t.pending = append(t.pending, c)
			t.pendBytes += len(c.turn) + len(c.status) + len(c.message)
		case t.turnID == id:
			s.finishTurn(c.status, c.message)
		default:
			s.unknown("turn/completed for another turn", "")
		}
		return
	}
	switch {
	case t.turnID == "":
		s.buffer(t, m, id, pk.Params)
	case t.turnID == id:
		s.applyContent(m, pk.Params)
	default:
		s.unknown(m+" for another turn", "")
	}
}

func (s *session) buffer(t *turn, method, id string, raw json.RawMessage) {
	if t.dropped {
		return
	}
	if len(t.frames) >= maxPendingFrames || len(raw)+len(id) > maxPendingBytes-t.frameBytes {
		t.frames, t.frameBytes, t.dropped = nil, 0, true
		s.unknown("frames arrived before the turn was bound and exceeded the buffer; they were dropped", "")
		return
	}
	t.frames = append(t.frames, frame{method: method, turn: id, raw: append(json.RawMessage(nil), raw...)})
	t.frameBytes += len(raw) + len(id)
}

// applyContent reports one frame of the bound turn.
func (s *session) applyContent(method string, raw json.RawMessage) {
	t := s.cur
	switch method {
	case "item/started", "item/completed":
		var p struct {
			Item map[string]any `json:"item"`
		}
		if json.Unmarshal(raw, &p) != nil || p.Item == nil {
			s.unknown(method+" carries no item", string(raw))
			return
		}
		typ, _ := p.Item["type"].(string)
		id, _ := p.Item["id"].(string)
		if method == "item/completed" && !s.acceptCompleted(id) {
			s.unknown("completed item with a missing, oversize, repeated or excess id", "")
			return
		}
		if typ == "agentMessage" {
			if method == "item/completed" {
				text, _ := p.Item["text"].(string)
				_ = s.em.Message(s.ctx, "assistant", clip(text, 2*harness.MaxText))
				t.answered = true
			}
			return
		}
		if name, input, status, ok := toolInfo(p.Item); ok {
			if s.control != nil && name != "file_change" {
				s.fail("codex: forbidden tool observed")
				return
			}
			if method == "item/started" {
				status = "requested"
			}
			_ = s.em.ToolCall(s.ctx, id, name, input, status)
			t.answered = true
			return
		}
		switch typ {
		case "userMessage", "reasoning", "plan", "functionCallOutput", "contextCompaction",
			"hookPrompt", "imageView", "sleep", "enteredReviewMode", "exitedReviewMode":
			return // restated elsewhere or internal to the harness
		}
		s.unknown("item type "+clip(typ, 64), string(raw))
	case "thread/tokenUsage/updated":
		var w struct {
			TokenUsage struct {
				Last struct {
					InputTokens     int64 `json:"inputTokens"`
					CachedInput     int64 `json:"cachedInputTokens"`
					CacheWriteInput int64 `json:"cacheWriteInputTokens"`
					OutputTokens    int64 `json:"outputTokens"`
				} `json:"last"`
			} `json:"tokenUsage"`
		}
		if json.Unmarshal(raw, &w) != nil {
			s.unknown("token usage does not decode", string(raw))
			return
		}
		l := w.TokenUsage.Last
		t.usage = &harness.Usage{Input: l.InputTokens, Output: l.OutputTokens, CacheCreate: l.CacheWriteInput, CacheRead: l.CachedInput}
	case "error":
		var p struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
			WillRetry bool `json:"willRetry"`
		}
		_ = json.Unmarshal(raw, &p)
		text := "codex error: " + clip(p.Error.Message, harness.MaxText)
		if s.control != nil {
			text = "codex error"
		}
		if p.WillRetry {
			text += " (retrying)"
		}
		_ = s.em.Message(s.ctx, "system", text)
	}
}

func (s *session) acceptCompleted(id string) bool {
	if id == "" || len(id) > maxItemIDBytes || s.completed[id] || len(s.completed) >= maxCompletedItems {
		return false
	}
	s.completed[id] = true
	return true
}

// toolInfo recognises items that are tool activity. The input is what the
// model asked to run; only its digest leaves the adapter.
func toolInfo(item map[string]any) (name string, input []byte, status string, ok bool) {
	typ, _ := item["type"].(string)
	str := func(k string) string { v, _ := item[k].(string); return v }
	js := func(k string) []byte { b, _ := json.Marshal(item[k]); return b }
	switch typ {
	case "commandExecution":
		name, input = "command", []byte(str("command"))
	case "fileChange":
		name, input = "file_change", js("changes")
	case "mcpToolCall":
		name, input = "mcp:"+str("server")+"/"+str("tool"), js("arguments")
	case "dynamicToolCall":
		name, input = "dynamic:"+str("tool"), js("arguments")
	case "webSearch":
		name, input = "web_search", []byte(str("query"))
	case "collabAgentToolCall":
		name, input = "agent:"+str("tool"), []byte(str("prompt"))
	default:
		return "", nil, "", false
	}
	switch str("status") {
	case "completed":
		status = "ok"
	case "failed":
		status = "error"
	case "declined":
		status = "denied"
	default:
		status = "requested"
	}
	return name, input, status, true
}

// finishTurn handles the completion of the bound turn.
func (s *session) finishTurn(status, message string) {
	t := s.cur
	if t.usage != nil {
		_ = s.em.Usage(s.ctx, *t.usage)
		t.usage = nil
	}
	s.cur = nil
	switch status {
	case "completed":
		if len(s.queue) > 0 {
			s.dispatch()
			return
		}
		if !s.opts.keepOpen {
			s.finish(harness.Final{Outcome: harness.OutcomeDone, ExitCode: -1})
		}
	case "interrupted":
		_ = s.em.Message(s.ctx, "system", "turn interrupted")
		s.dispatch()
	case "failed":
		reason := "turn failed"
		if message != "" && s.control == nil {
			reason += ": " + clip(message, 512)
		}
		s.finish(harness.Final{Outcome: harness.OutcomeFailed, Reason: reason, ExitCode: -1})
	default:
		s.finish(harness.Final{Outcome: harness.OutcomeFailed, Reason: "turn ended with unrecognised status " + clip(status, 32), ExitCode: -1})
	}
}

// ---- unknown, final, teardown ----

// unknown reports something that could not be classified. Beyond the cap the
// events are counted and the count is carried by Final.
func (s *session) unknown(why, raw string) {
	if s.control != nil {
		why = "unclassified Codex protocol frame"
		raw = ""
	}
	if s.unknownN >= maxUnknownEvents {
		s.suppressed++
		return
	}
	s.unknownN++
	_ = s.em.Unknown(s.ctx, why, clip(raw, 4*harness.MaxUnknownRaw))
}

func (s *session) fail(reason string) {
	s.finish(harness.Final{Outcome: harness.OutcomeFailed, Reason: reason, ExitCode: -1})
}

func (s *session) undelivered() int {
	n := len(s.queue)
	if s.cur != nil && !s.cur.answered {
		n++
	}
	return n
}

// finish emits the one Final and stops the process. When the process exited
// by itself, its exit status is the Final's.
func (s *session) finish(f harness.Final) {
	if s.finished {
		return
	}
	s.finished = true
	s.disarm()
	if f.Outcome != harness.OutcomeDone {
		if n := s.undelivered(); n > 0 {
			f.Reason = joinReason(f.Reason, fmt.Sprintf("undelivered_inputs=%d", n))
		}
	}
	if s.suppressed > 0 {
		f.Reason = joinReason(f.Reason, fmt.Sprintf("unknown_events_suppressed=%d", s.suppressed))
	}
	if err := s.teardown(); err != nil {
		f.Outcome = harness.OutcomeFailed
		f.Reason = joinReason(f.Reason, "owned process cleanup failed")
	}
	if strings.HasPrefix(f.Reason, "codex exited without a result:") && s.p.cmd.ProcessState != nil {
		f.ExitCode = s.p.cmd.ProcessState.ExitCode()
		if s.control != nil {
			f.Reason = fmt.Sprintf("codex exited without a result (code %d)", f.ExitCode)
		} else {
			f.Reason = "codex exited without a result: " + exitText(f.ExitCode, s.p.waitErr)
		}
	}
	_ = s.em.Final(s.ctx, f)
	if s.em.Err() != nil && f.Outcome == harness.OutcomeDone {
		f.Outcome = harness.OutcomeFailed
		f.Reason = joinReason(f.Reason, "event sink failed: "+s.em.Err().Error())
	}
	s.mu.Lock()
	s.final = f
	s.mu.Unlock()
}

func joinReason(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

// teardown ends the process group and every goroutine of the session.
func (s *session) teardown() error {
	close(s.quit)
	_ = s.p.stdin.Close()
	err := s.p.cleanup(s.grace)
	_ = s.p.stdout.Close()
	s.wg.Wait()
	return err
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
