package claudecode

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sync"
)

// FixedWrite is a one-use native permission callback. It grants no permission
// updates and never returns provider text, tool arguments, or a caller's path.
// It does not sandbox the process: a reviewed native launcher is required.
type FixedWrite struct {
	Path          string
	RequiredHooks map[string]int
	mu            sync.Mutex
	seen          map[string]bool
	approved      bool
	failed        bool
}

var controlID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func (f *FixedWrite) Approved() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.approved && !f.failed
}

func (f *FixedWrite) refuse() { f.mu.Lock(); f.failed = true; f.mu.Unlock() }

func cleanAbsentArtifact(p string) bool {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p || filepath.Base(p) != "demo.txt" {
		return false
	}
	if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
		return false
	}
	for d := filepath.Dir(p); ; d = filepath.Dir(d) {
		st, err := os.Lstat(d)
		if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return false
		}
		if d == filepath.Dir(d) {
			break
		}
	}
	return true
}

// strictJSON rejects duplicate keys at every depth, excessive nesting, and
// trailing data before the typed reducer sees any field.
func strictJSON(b []byte, dst any) error {
	if len(b) > 64<<10 {
		return errors.New("controlled frame too large")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	var value func(int) error
	value = func(depth int) error {
		if depth > 16 {
			return errors.New("controlled frame too deep")
		}
		tok, err := d.Token()
		if err != nil {
			return err
		}
		if delim, ok := tok.(json.Delim); ok {
			switch delim {
			case '{':
				seen := map[string]bool{}
				for d.More() {
					k, e := d.Token()
					if e != nil {
						return e
					}
					key, ok := k.(string)
					if !ok || seen[key] {
						return errors.New("duplicate controlled field")
					}
					seen[key] = true
					if e = value(depth + 1); e != nil {
						return e
					}
				}
				end, e := d.Token()
				if e != nil || end != json.Delim('}') {
					return errors.New("invalid controlled object")
				}
			case '[':
				for d.More() {
					if e := value(depth + 1); e != nil {
						return e
					}
				}
				end, e := d.Token()
				if e != nil || end != json.Delim(']') {
					return errors.New("invalid controlled array")
				}
			default:
				return errors.New("unexpected controlled delimiter")
			}
		}
		return nil
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing controlled data")
	}
	return json.Unmarshal(b, dst)
}

type permissionFrame struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id"`
	Request   struct {
		Subtype   string                     `json:"subtype"`
		ToolName  string                     `json:"tool_name"`
		Input     map[string]json.RawMessage `json:"input"`
		ToolUseID string                     `json:"tool_use_id"`
		AgentID   string                     `json:"agent_id"`
	} `json:"request"`
}

// decide consumes a bounded native control frame. Unknown/duplicate/subagent
// requests poison the job even when native code subsequently reports success.
func (f *FixedWrite) decide(line []byte, pending map[string]toolInfo) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	bad := func() (any, error) {
		f.failed = true
		return nil, errors.New("claudecode: controlled permission request refused")
	}
	var r permissionFrame
	if strictJSON(line, &r) != nil || r.Type != "control_request" || !controlID.MatchString(r.RequestID) {
		return bad()
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(line, &fields)
	for k := range fields {
		if k != "type" && k != "request_id" && k != "request" {
			return bad()
		}
	}
	var requestFields map[string]json.RawMessage
	_ = json.Unmarshal(fields["request"], &requestFields)
	allowed := map[string]bool{"subtype": true, "tool_name": true, "input": true, "permission_suggestions": true, "blocked_path": true, "decision_reason": true, "title": true, "display_name": true, "description": true, "tool_use_id": true, "agent_id": true}
	for k := range requestFields {
		if !allowed[k] {
			return bad()
		}
	}
	if f.seen == nil {
		f.seen = map[string]bool{}
	}
	if f.seen[r.RequestID] {
		return bad()
	}
	f.seen[r.RequestID] = true
	reply := map[string]any{"behavior": "deny", "message": "outside fixed owned file task", "interrupt": true}
	allow := r.Request.Subtype == "can_use_tool" && r.Request.ToolName == "Write" && r.Request.AgentID == "" && controlID.MatchString(r.Request.ToolUseID) && len(r.Request.Input) == 2 && !f.approved && !f.failed && cleanAbsentArtifact(f.Path)
	var path, content string
	allow = allow && json.Unmarshal(r.Request.Input["file_path"], &path) == nil && json.Unmarshal(r.Request.Input["content"], &content) == nil && path == f.Path && content == "fabric\n"
	ti, observed := pending[r.Request.ToolUseID]
	var original map[string]json.RawMessage
	var originalPath, originalContent string
	allow = allow && observed && ti.name == "Write" && strictJSON(ti.input, &original) == nil && len(original) == 2 && json.Unmarshal(original["file_path"], &originalPath) == nil && json.Unmarshal(original["content"], &originalContent) == nil && originalPath == f.Path && originalContent == "fabric\n"
	if allow {
		f.approved = true
		reply = map[string]any{"behavior": "allow", "updatedInput": map[string]string{"file_path": f.Path, "content": "fabric\n"}}
	} else {
		f.failed = true
	}
	return map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": r.RequestID, "response": reply}}, nil
}
