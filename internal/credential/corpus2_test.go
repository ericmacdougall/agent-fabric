package credential

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ac9Diagnosis is W2's AC-10 must-pass canary diagnosis (goodqa#15966,
// origin/feat/15966-w2-effect internal/m1comment/m1comment_test.go
// TestScanPassesRealDiagnosis), copied verbatim.
const ac9Diagnosis = `The failing test is test.sh, which calls sum_to from lib.sh.
In /srv/fabric/runs/r_01JD8XK2Q7M4N6P8R0S2T4V6W8Y0Z1A3/wt/lib.sh line 7 the loop runs
"for ((i = 1; i < n; i++))", so sum_to 5 returns 10 instead of 15: an off-by-one.
/srv/fabric/runs/r_01JD8XK2Q7M4N6P8R0S2T4V6W8Y0Z1A3/wt/test.sh lines 4-9 assert sum_to 1, 5 and 10.
Fix: use i <= n. Mission m_canary-m1, attempt r_01JD8XK2Q7M4N6P8R0S2T4V6W8Y0Z1A3,
commit 8297e422ad519e23152e7a21119ed9bc5c8991ad. Output: FAIL sum_to 5: want 15, got 10.`

// corpus2 is the goodqa#16040 second corpus (at least 1 MB): claude
// stream-json lines carrying the AC-9 diagnosis, the repository's own Go
// sources as Read tool results, and `go test -json` events with commit
// shas, package paths and durations. It is deterministic.
func corpus2(t testing.TB, minBytes int) [][]byte {
	t.Helper()
	var files []string
	filepath.WalkDir("../..", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "testdata" || d.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(p, ".go") {
			files = append(files, p)
		}
		return nil
	})
	sort.Strings(files)
	line := func(v any) []byte { b, _ := json.Marshal(v); return append(b, '\n') }
	var out [][]byte
	var cur []byte
	total := 0
	flush := func() {
		if len(cur) > 0 {
			out = append(out, cur)
			total += len(cur)
			cur = nil
		}
	}
	t0 := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	for i, p := range files {
		if total >= minBytes {
			break
		}
		src, err := os.ReadFile(p)
		if err != nil || len(src) > 64<<10 {
			continue
		}
		id := fmt.Sprintf("toolu_%024x", sha1.Sum([]byte(p)))[:30]
		cur = append(cur, line(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_use", "id": id, "name": "Read", "input": map[string]any{"file_path": "/srv/fabric/runs/r_01JD8XK2Q7M4N6P8R0S2T4V6W8Y0Z1A3/wt/" + p[6:]}}}}})...)
		cur = append(cur, line(map[string]any{"type": "user", "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": id, "content": string(src)}}}})...)
		// go test -json events for the file's package.
		pkg := "fabric/" + filepath.ToSlash(filepath.Dir(p[6:]))
		sum := sha1.Sum(src)
		for j, act := range []string{"start", "run", "output", "pass"} {
			ev := map[string]any{"Time": t0.Add(time.Duration(i*4+j) * 137 * time.Millisecond).Format(time.RFC3339Nano), "Action": act, "Package": pkg}
			if act != "start" {
				ev["Test"] = "Test" + strings.TrimSuffix(filepath.Base(p), ".go")
			}
			if act == "output" {
				ev["Output"] = fmt.Sprintf("    %s:%d: built at %s in %ss\n", filepath.Base(p), 10+i%90, hex.EncodeToString(sum[:]), strconv.FormatFloat(float64(i%997)/100, 'f', 2, 64))
			}
			if act == "pass" {
				ev["Elapsed"] = float64(i%500) / 100
			}
			cur = append(cur, line(ev)...)
		}
		if i%40 == 0 {
			cur = append(cur, line(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": ac9Diagnosis}}}})...)
			flush()
		}
	}
	cur = append(cur, line(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": ac9Diagnosis})...)
	flush()
	if total < minBytes {
		t.Fatalf("corpus2 only %d bytes", total)
	}
	return out
}

func TestRedactionFalsePositiveCorpus2(t *testing.T) {
	n := 20 // quick local smoke; the pre-registered run sets FAB_PROP2_N
	if v := os.Getenv("FAB_PROP2_N"); v != "" {
		var err error
		if n, err = strconv.Atoi(v); err != nil || n <= 0 {
			t.Fatalf("FAB_PROP2_N=%q", v)
		}
	}
	streams := corpus2(t, 1<<20)
	size := 0
	for _, s := range streams {
		size += len(s)
	}
	start := time.Now()
	c := propertyRun(t, streams, n, false)
	t.Logf("goodqa#16040 corpus2: bytes=%d streams=%d n=%d k_block=%d k_rewrite_clean=%d k_exposed_clean=%d seeded_rewritten=%d wilson95_upper(k_block)=%.6g elapsed=%s",
		size, len(streams), c.n, c.kBlock, c.kRewriteClean, c.kExposedClean, c.seededRewritten, WilsonUpper(c.kBlock, c.n, 1.96), time.Since(start).Round(time.Millisecond))
	if c.kExposedClean > 0 {
		t.Fatalf("corpus2: %d of %d runs raised credential_exposure on clean streams", c.kExposedClean, c.n)
	}
	if c.kBlock > 0 && WilsonUpper(c.kBlock, c.n, 1.96) > 1e-3 {
		t.Fatalf("corpus2 blocking hits: k_block=%d of %d", c.kBlock, c.n)
	}
}
