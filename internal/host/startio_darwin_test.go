//go:build darwin

package host

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// goodqa#15373 AC-4 (Darwin): StartIO hands a 0600 regular-file stdin to a
// child under sandbox-exec; the child sees a regular file on fd 0 and reads
// it through the inherited descriptor (cat on fd 0, never /dev/stdin).
func TestStartIODarwin(t *testing.T) {
	d := &Darwin{SandboxExec: lookSandbox(t)}
	g := controlGrant(t.TempDir())
	g.Profile = "P0"
	p := filepath.Join(g.RunDir, "prompt.jsonl")
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("canary-15373-darwin\n") //nolint:errcheck
	f.Close()
	in, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.CreateTemp(g.RunDir, "out")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	h, err := d.StartIO(g, []string{"/bin/sh", "-c", `stat -L -f %HT /dev/fd/0; cat`}, nil, in, out, out)
	if err != nil {
		t.Fatal(err)
	}
	if code, err := d.Wait(h); err != nil || code != 0 {
		b, _ := os.ReadFile(out.Name())
		t.Fatalf("wait: code %d err %v; output %q", code, err, b)
	}
	b, _ := os.ReadFile(out.Name())
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) < 2 || lines[0] != "Regular File" || lines[1] != "canary-15373-darwin" {
		t.Fatalf("want a regular-file stdin echoed through fd 0, got %q", b)
	}
}
