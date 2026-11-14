package sweep

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"fabric/internal/credential"
)

// TestGeneratePerfWorkload writes the AC-16 (d) workload when
// FAB_SWEEP_PERF_DIR is set (it is skipped otherwise): 8 synthetic 512-byte
// credentials giving 144 patterns (9 forms x 2 normalisations, no
// deduplication), a run area of exactly 20000 entries and 512 MiB, and
// every pattern planted once in the last file in traversal order. It writes
// input.json and expected.json beside root/. The measurement runs the real
// `fab __sweep` under the target's CPU and memory limits.
func TestGeneratePerfWorkload(t *testing.T) {
	dir := os.Getenv("FAB_SWEEP_PERF_DIR")
	if dir == "" {
		t.Skip("set FAB_SWEEP_PERF_DIR to generate the AC-16 (d) workload")
	}
	const alpha = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	key := make([]byte, 32)
	rand.Read(key)
	base := uint64(1<<32) + 12345678901
	in := credential.SweepInput{Key: base64.StdEncoding.EncodeToString(key), Base: base, Window: 32, Literal: credential.PublicLiteral}
	m := hmac.New(sha256.New, key)
	var planted []byte
	expected := map[string]int{}
	for v := 0; v < 8; v++ {
		val := make([]byte, 512)
		for i := range val {
			x, _ := rand.Int(rand.Reader, big.NewInt(int64(len(alpha))))
			val[i] = alpha[x.Int64()]
		}
		val[100] = '<'  // makes the two JSON forms differ, so 9 distinct forms
		val[200] = '\\' // with the next byte, a raw \n: N_esc differs from N_ws
		val[201] = 'n'
		for _, f := range credential.AllForms {
			enc := credential.Encode(f, val)
			for _, n := range credential.AllNorms {
				p := credential.NormalizeBytes(n, enc)
				label := fmt.Sprintf("claude-oauth:v%d:%s-%s", v, f, n)
				m.Reset()
				m.Write(p)
				in.Entries = append(in.Entries, credential.SweepEntry{Len: len(p), FP: credential.Fingerprint(p, base, 32),
					MAC: base64.StdEncoding.EncodeToString(m.Sum(nil)), Norm: n, Label: label})
				planted = append(planted, '\n', '\n')
				planted = append(planted, p...)
				expected[label]++
			}
		}
	}
	// Patterns that are identical across norms are matched by both entries.
	data, _ := json.Marshal(in)
	if len(in.Entries) != 144 || len(data) > credential.SweepMaxInput {
		t.Fatalf("entries %d, input %d bytes", len(in.Entries), len(data))
	}
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(dir, "input.json"), data, 0o600))
	root := filepath.Join(dir, "root")
	must(os.MkdirAll(root, 0o755))
	const dirs, entries, total = 20, 20000, 512 << 20
	files := entries - dirs
	size := total / files
	last := total - size*(files-1)
	if last < len(planted) {
		t.Fatal("planted payload too big")
	}
	buf := make([]byte, last)
	fill := func(b []byte) { // base64 text: realistic, high-entropy, not matching
		rand.Read(b)
		for i := range b {
			b[i] = alpha[int(b[i])&63]
		}
	}
	k := 0
	for d := 0; d < dirs; d++ {
		sub := filepath.Join(root, fmt.Sprintf("d%02d", d))
		must(os.Mkdir(sub, 0o755))
		per := files / dirs
		for i := 0; i < per; i++ {
			k++
			n := size
			if k == files {
				n = last
			}
			b := buf[:n]
			fill(b)
			if k == files {
				copy(b[n-len(planted):], planted)
			}
			must(os.WriteFile(filepath.Join(sub, fmt.Sprintf("f%05d", i)), b, 0o600))
		}
	}
	exp, _ := json.Marshal(map[string]any{"counts": expected, "files": files, "bytes": total})
	must(os.WriteFile(filepath.Join(dir, "expected.json"), exp, 0o600))
}
