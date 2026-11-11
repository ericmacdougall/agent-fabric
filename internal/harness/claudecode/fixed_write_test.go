package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func permissionLine(path, content, tool, agent string) []byte {
	b, _ := json.Marshal(map[string]any{"type": "control_request", "request_id": "request1", "request": map[string]any{"subtype": "can_use_tool", "tool_name": tool, "agent_id": agent, "tool_use_id": "tool1", "input": map[string]string{"file_path": path, "content": content}}})
	return b
}
func fixedPending(path string) map[string]toolInfo {
	b, _ := json.Marshal(map[string]string{"file_path": path, "content": "fabric\n"})
	return map[string]toolInfo{"tool1": {name: "Write", input: b}}
}

func TestFixedWriteOneExactApproval(t *testing.T) {
	p := filepath.Join(t.TempDir(), "demo.txt")
	f := &FixedWrite{Path: p}
	response, err := f.decide(permissionLine(p, "fabric\n", "Write", ""), fixedPending(p))
	b, _ := json.Marshal(response)
	if err != nil || !f.Approved() || !strings.Contains(string(b), `"behavior":"allow"`) || strings.Contains(string(b), "updatedPermissions") {
		t.Fatalf("approval missing: %v %s", err, b)
	}
	if _, err := f.decide(permissionLine(p, "fabric\n", "Write", ""), fixedPending(p)); err == nil || f.Approved() {
		t.Fatal("duplicate request accepted")
	}
}

func TestFixedWriteRefusesBoundaries(t *testing.T) {
	p := filepath.Join(t.TempDir(), "demo.txt")
	for _, tc := range []struct {
		name, path, content, tool, agent string
		pending                          bool
	}{
		{"sibling", filepath.Join(filepath.Dir(p), "other"), "fabric\n", "Write", "", true},
		{"wrongContent", p, "extra", "Write", "", true}, {"shell", p, "fabric\n", "Bash", "", true},
		{"read", p, "fabric\n", "Read", "", true},
		{"subagent", p, "fabric\n", "Write", "agent1", true}, {"unbound", p, "fabric\n", "Write", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &FixedWrite{Path: p}
			pending := fixedPending(p)
			if !tc.pending {
				pending = nil
			}
			v, err := f.decide(permissionLine(tc.path, tc.content, tc.tool, tc.agent), pending)
			b, _ := json.Marshal(v)
			if err != nil || f.Approved() || !strings.Contains(string(b), `"behavior":"deny"`) {
				t.Fatalf("unsafe approval %v %s", err, b)
			}
		})
	}
	if err := os.WriteFile(p, []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}
	f := &FixedWrite{Path: p}
	v, _ := f.decide(permissionLine(p, "fabric\n", "Write", ""), fixedPending(p))
	b, _ := json.Marshal(v)
	if f.Approved() || !strings.Contains(string(b), `"behavior":"deny"`) {
		t.Fatal("existing target accepted")
	}
}

func TestFixedWriteRejectsMalformedWithoutReflection(t *testing.T) {
	p := filepath.Join(t.TempDir(), "demo.txt")
	for _, line := range []string{`{"type":"control_request","type":"public-canary"}`, `{"a":{"x":1,"x":2}}`, `{"type":`, strings.Repeat("public-canary", 7000), `{} {}`} {
		f := &FixedWrite{Path: p}
		v, err := f.decide([]byte(line), fixedPending(p))
		if err == nil || v != nil || f.Approved() || strings.Contains(err.Error(), "public-canary") {
			t.Fatal("malformed reflected or accepted")
		}
	}
}

func TestFixedWriteSymlinkRefused(t *testing.T) {
	d := t.TempDir()
	target := filepath.Join(d, "target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(d, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if cleanAbsentArtifact(filepath.Join(link, "demo.txt")) {
		t.Fatal("symlink ancestry accepted")
	}
}
