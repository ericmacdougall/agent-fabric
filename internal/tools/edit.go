package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"golang.org/x/sys/unix"

	"fabric/internal/agent"
)

// Edit replaces unique text spans in one existing file inside the worktree.
type Edit struct{ core fsCore }

// Write creates or replaces one file inside the worktree.
type Write struct{ core fsCore }

var (
	_ agent.Tool           = (*Edit)(nil)
	_ agent.Tool           = (*Write)(nil)
	_ agent.InterruptNoter = (*Edit)(nil)
	_ agent.InterruptNoter = (*Write)(nil)
)

// NewEdit and NewWrite share rd's root and path rules. pin is required;
// keeper may be nil (no pre-image copies).
func NewEdit(rd *Read, pin *WorktreePin, keeper *PreimageKeeper) *Edit {
	return &Edit{fsCore{read: rd, Pin: pin, Preimage: keeper}}
}

func NewWrite(rd *Read, pin *WorktreePin, keeper *PreimageKeeper) *Write {
	return &Write{fsCore{read: rd, Pin: pin, Preimage: keeper}}
}

func (*Edit) Name() string         { return "edit" }
func (*Edit) SideEffecting() bool  { return true }
func (*Write) Name() string        { return "write" }
func (*Write) SideEffecting() bool { return true }
func (*Edit) Def() json.RawMessage {
	return json.RawMessage(`{"name":"edit","description":"Replace text in an existing file inside the worktree. Each oldText must match exactly once in the original file; edits must not overlap. Optional expect_sha256 refuses the edit if the file changed.","input_schema":{"type":"object","properties":{"path":{"type":"string"},"edits":{"type":"array","minItems":1,"maxItems":64,"items":{"type":"object","properties":{"oldText":{"type":"string"},"newText":{"type":"string"}},"required":["oldText","newText"]}},"expect_sha256":{"type":"string"}},"required":["path","edits"]}}`)
}
func (*Write) Def() json.RawMessage {
	return json.RawMessage(`{"name":"write","description":"Create or replace a file inside the worktree (at most 1 MiB). Missing parent directories are created. Optional expect_sha256 refuses the write if the existing file changed.","input_schema":{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"},"expect_sha256":{"type":"string"}},"required":["path","content"]}}`)
}

type editIn struct {
	Path   string `json:"path"`
	Edits  []span `json:"edits"`
	Expect string `json:"expect_sha256"`
}

type span struct {
	Old string `json:"oldText"`
	New string `json:"newText"`
}

func decodeStrict(input json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func (e *Edit) InterruptedNote(callID string, input json.RawMessage) string {
	var in editIn
	if json.Unmarshal(input, &in) != nil {
		return ""
	}
	return e.core.recoverText(callID, in.Path)
}

func (w *Write) InterruptedNote(callID string, input json.RawMessage) string {
	var in writeIn
	if json.Unmarshal(input, &in) != nil {
		return ""
	}
	return w.core.recoverText(callID, in.Path)
}

type match struct {
	s, e     int // original byte span
	newBytes []byte
}

func (e *Edit) Run(ctx context.Context, input json.RawMessage) (string, bool) {
	var in editIn
	if err := decodeStrict(input, &in); err != nil || in.Path == "" {
		return `edit needs {"path": "relative/file", "edits": [{"oldText": "...", "newText": "..."}]}`, true
	}
	out, err := e.edit(ctx, in)
	if errors.Is(err, errAfterRename) {
		return out, true
	}
	if err != nil {
		return "edit refused: " + err.Error(), true
	}
	return out, false
}

func (e *Edit) edit(ctx context.Context, in editIn) (string, error) {
	if n := len(in.Edits); n < 1 || n > maxEdits {
		return "", fmt.Errorf("edits must hold 1 to %d entries", maxEdits)
	}
	for i, ed := range in.Edits {
		if ed.Old == "" {
			return "", fmt.Errorf("edit %d: oldText is empty", i+1)
		}
		if ed.Old == ed.New {
			return "", fmt.Errorf("edit %d: oldText equals newText", i+1)
		}
		if len(ed.Old) > MaxFileBytes || len(ed.New) > MaxFileBytes {
			return "", fmt.Errorf("edit %d is larger than %d bytes", i+1, MaxFileBytes)
		}
	}
	if err := validExpect(in.Expect); err != nil {
		return "", err
	}
	c := &e.core
	parts, err := c.resolve(in.Path)
	if err != nil {
		return "", err
	}
	parent, made, err := c.parentDir(parts, false)
	if err != nil {
		return "", err
	}
	defer func() { unix.Close(parent); releaseDirs(made, false) }()
	name := parts[len(parts)-1]
	pre, err := loadTarget(parent, name)
	if err != nil {
		return "", err
	}
	if !pre.exists {
		return "", errors.New("no such file (use write to create one)")
	}
	if in.Expect != "" && in.Expect != pre.sha {
		return "", fmt.Errorf("expect_sha256 does not match the file (current sha256 %s)", pre.sha)
	}
	matches, err := locate(pre.data, in.Edits)
	if err != nil {
		return "", err
	}
	var res bytes.Buffer
	last := 0
	// matches are sorted ascending; build the result from the original bytes
	// (equivalent to applying from the highest offset down).
	for _, m := range matches {
		res.Write(pre.data[last:m.s])
		res.Write(m.newBytes)
		last = m.e
	}
	res.Write(pre.data[last:])
	if res.Len() > MaxFileBytes {
		return "", fmt.Errorf("result would be larger than %d bytes", MaxFileBytes)
	}
	if bytes.Equal(res.Bytes(), pre.data) {
		return "", errors.New("the edits change nothing")
	}
	return c.finishWrite(ctx, parts, parent, name, pre, res.Bytes(), "edited")
}

func validExpect(s string) error {
	if s == "" || s == "new" {
		return nil
	}
	if len(s) != 64 {
		return errors.New("expect_sha256 must be 64 lowercase hex characters")
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return errors.New("expect_sha256 must be 64 lowercase hex characters")
		}
	}
	return nil
}

// locate finds every edit in the ORIGINAL bytes: exact first, normalised only
// when there is no exact match. Spans are mapped back to original offsets.
func locate(orig []byte, edits []span) ([]match, error) {
	crlf := bytes.Count(orig, []byte("\r\n"))*2 > bytes.Count(orig, []byte{'\n'}) && bytes.Contains(orig, []byte("\r\n"))
	var nt *normText
	var ms []match
	for i, ed := range edits {
		oldB, newB := []byte(ed.Old), []byte(ed.New)
		if crlf {
			oldB, newB = toCRLF(oldB), toCRLF(newB)
		}
		hits := findAll(orig, oldB)
		var s, en int
		switch len(hits) {
		case 1:
			s, en = hits[0], hits[0]+len(oldB)
		case 0:
			if nt == nil {
				n := normalize(orig)
				nt = &n
			}
			no := normalize(oldB).b
			if len(no) == 0 {
				return nil, fmt.Errorf("edit %d: oldText matches nothing", i+1)
			}
			nh := findAll(nt.b, no)
			if len(nh) == 0 {
				return nil, fmt.Errorf("edit %d: oldText matches nothing", i+1)
			}
			if len(nh) > 1 {
				return nil, fmt.Errorf("edit %d: oldText matches %d places (normalised, lines %s); add context so it is unique", i+1, len(nh), normLines(orig, nt, nh))
			}
			s, en = nt.start[nh[0]], nt.end[nh[0]+len(no)-1]
		default:
			return nil, fmt.Errorf("edit %d: oldText matches %d places (lines %s); add context so it is unique", i+1, len(hits), lines(orig, hits))
		}
		ms = append(ms, match{s: s, e: en, newBytes: newB})
	}
	sort.Slice(ms, func(a, b int) bool { return ms[a].s < ms[b].s })
	for i := 1; i < len(ms); i++ {
		if ms[i].s < ms[i-1].e {
			return nil, errors.New("edits overlap")
		}
	}
	return ms, nil
}

func toCRLF(b []byte) []byte {
	b = bytes.ReplaceAll(b, []byte("\r\n"), []byte{'\n'})
	return bytes.ReplaceAll(b, []byte{'\n'}, []byte("\r\n"))
}

func lines(b []byte, offs []int) string {
	var ls []string
	for i, o := range offs {
		if i == 10 {
			ls = append(ls, "...")
			break
		}
		ls = append(ls, fmt.Sprint(lineOf(b, o)))
	}
	return strings.Join(ls, ",")
}

func normLines(orig []byte, nt *normText, offs []int) string {
	var o []int
	for _, x := range offs {
		o = append(o, nt.start[x])
	}
	return lines(orig, o)
}

// ---- write ----

type writeIn struct {
	Path    string  `json:"path"`
	Content *string `json:"content"`
	Expect  string  `json:"expect_sha256"`
}

func (w *Write) Run(ctx context.Context, input json.RawMessage) (string, bool) {
	var in writeIn
	if err := decodeStrict(input, &in); err != nil || in.Path == "" || in.Content == nil {
		return `write needs {"path": "relative/file", "content": "..."}`, true
	}
	out, err := w.write(ctx, in)
	if errors.Is(err, errAfterRename) {
		return out, true
	}
	if err != nil {
		return "write refused: " + err.Error(), true
	}
	return out, false
}

func (w *Write) write(ctx context.Context, in writeIn) (string, error) {
	data := []byte(*in.Content)
	if len(data) > MaxFileBytes {
		return "", fmt.Errorf("content is larger than %d bytes", MaxFileBytes)
	}
	if err := validExpect(in.Expect); err != nil {
		return "", err
	}
	c := &w.core
	parts, err := c.resolve(in.Path)
	if err != nil {
		return "", err
	}
	parent, made, err := c.parentDir(parts, true)
	if err != nil {
		return "", err
	}
	ok := false
	defer func() { unix.Close(parent); releaseDirs(made, !ok) }()
	name := parts[len(parts)-1]
	pre, err := loadTarget(parent, name)
	if err != nil {
		return "", err
	}
	if in.Expect != "" {
		cur := "new"
		if pre.exists {
			cur = pre.sha
		}
		if in.Expect != cur {
			return "", fmt.Errorf("expect_sha256 does not match the file (current %s)", cur)
		}
	}
	out, err := c.finishWrite(ctx, parts, parent, name, pre, data, "wrote")
	ok = err == nil || errors.Is(err, errAfterRename)
	return out, err
}

var errAfterRename = errors.New("worktree moved after the write")

// finishWrite keeps the pre-image copy, commits and renders the result.
func (c *fsCore) finishWrite(ctx context.Context, parts []string, parent int, name string, pre snapshot, data []byte, verb string) (string, error) {
	if testAfterLoad != nil {
		testAfterLoad()
	}
	rel := strings.Join(parts, "/")
	copyName, gitBlob := "", ""
	if pre.exists {
		need, blob := c.Preimage.NeedsCopy(ctx, rel, pre.data)
		if need {
			n, err := c.Preimage.Keep(pre.data)
			if err != nil {
				// Refused before the rename: the file is unchanged.
				return "", fmt.Errorf("refused, the file is unchanged: cannot keep a pre-image copy: %w", err)
			}
			copyName = n
		}
		gitBlob = blob
	}
	tag := callTag(ctx)
	if err := c.commit(tag, rel, parent, name, pre, data); err != nil {
		return "", err
	}
	post := sha256hex(data)
	preSha := "new"
	if pre.exists {
		preSha = pre.sha
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\npre_sha256: %s\npost_sha256: %s\nbytes: %d -> %d\n", verb, rel, preSha, post, len(pre.data), len(data))
	if copyName != "" {
		fmt.Fprintf(&b, "preimage_copy: %s\n", copyName)
	} else if gitBlob != "" {
		fmt.Fprintf(&b, "preimage_git_blob: %s\n", gitBlob)
	}
	b.WriteString("diff:\n" + unifiedDiff(rel, pre.data, data, MaxDiffBytes))
	if err := c.finish(); err != nil {
		return b.String() + "\nWARNING: the write above landed, but " + err.Error(), errAfterRename
	}
	return b.String(), nil
}
