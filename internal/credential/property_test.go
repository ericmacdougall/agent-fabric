package credential

import (
	"crypto/rand"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// WilsonUpper is the Wilson score interval upper bound for k of n at z.
func WilsonUpper(k, n int, z float64) float64 {
	if n == 0 {
		return 1
	}
	p := float64(k) / float64(n)
	nf := float64(n)
	den := 1 + z*z/nf
	centre := p + z*z/(2*nf)
	margin := z * math.Sqrt(p*(1-p)/nf+z*z/(4*nf*nf))
	return (centre + margin) / den
}

func TestWilsonUpper(t *testing.T) {
	// k=0, n=10000: z^2/(n+z^2).
	if got := WilsonUpper(0, 10000, 1.96); math.Abs(got-3.8401/10003.8416) > 1e-6 {
		t.Fatalf("wilson(0,10000) = %g", got)
	}
	if WilsonUpper(3, 10000, 1.96) > 1e-3 || WilsonUpper(4, 10000, 1.96) <= 1e-3 {
		t.Fatal("the pre-registered pass line is k_block <= 3 at n = 10000")
	}
}

func capturedStreams(t *testing.T) [][]byte {
	t.Helper()
	var paths []string
	for _, g := range []string{"../sched/testdata/streams/*/stream.jsonl", "../provider/claude/testdata/*.jsonl"} {
		m, _ := filepath.Glob(g)
		paths = append(paths, m...)
	}
	if len(paths) < 5 {
		t.Fatalf("only %d captured streams found", len(paths))
	}
	var out [][]byte
	for _, p := range paths {
		if fi, err := os.Stat(p); err != nil || !fi.Mode().IsRegular() {
			continue // e.g. the stream_unreadable fixture is a directory
		}
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b)
	}
	return out
}

type propCounts struct {
	n, kBlock, kRewrite, kRewriteClean, seededRewritten int
}

// propertyRun is the goodqa#16040 property: blocking hits and 'X' rewrites
// are counted apart. rewritesAsBlocks is the broken variant.
func propertyRun(t *testing.T, streams [][]byte, n int, rewritesAsBlocks bool) propCounts {
	t.Helper()
	c := propCounts{n: n}
	for i := 0; i < n; i++ {
		l, _ := rand.Int(rand.Reader, big.NewInt(121))
		tok := synthToken(t, 80+int(l.Int64()))
		b, err := NewBound(NewSecret(KindToken, "v1", tok),
			NewSecret(KindGoodQA, "g", synthSecret(t, 40)), NewSecret(KindDSN, "d", synthSecret(t, 24)))
		if err != nil {
			t.Fatal(err)
		}
		blocked, rewrote := false, false
		for _, s := range streams {
			r := b.Redact(s, nil)
			if r.Blocking > 0 || r.Blocked != "" {
				blocked = true
			}
			if r.Rewrites > 0 {
				rewrote = true
			}
		}
		if rewrote {
			c.kRewriteClean++
		}
		// NEG: a seeded real token fragment is always redacted, never
		// treated as benign and never a block.
		seeded := b.Redact(jsonLine("note "+string(tok[20:32])+" end"), nil)
		if seeded.Rewrites < 12 || seeded.Blocked != "" {
			t.Fatalf("run %d: seeded fragment rewrites=%d blocked=%q", i, seeded.Rewrites, seeded.Blocked)
		}
		c.seededRewritten++
		rewrote = true
		if rewritesAsBlocks && rewrote {
			blocked = true
		}
		if blocked {
			c.kBlock++
		}
		if rewrote {
			c.kRewrite++
		}
	}
	return c
}

func TestRedactionFalsePositiveProperty(t *testing.T) {
	n := 300 // quick local smoke; the pre-registered run sets FAB_PROP_N=10000
	if v := os.Getenv("FAB_PROP_N"); v != "" {
		var err error
		if n, err = strconv.Atoi(v); err != nil || n <= 0 {
			t.Fatalf("FAB_PROP_N=%q", v)
		}
	}
	streams := capturedStreams(t)
	c := propertyRun(t, streams, n, false)
	ub := WilsonUpper(c.kBlock, c.n, 1.96)
	t.Logf("goodqa#16040: streams=%d n=%d k_block=%d k_rewrite=%d k_rewrite_clean=%d seeded_rewritten=%d wilson95_upper(k_block)=%.6g threshold=1e-3",
		len(streams), c.n, c.kBlock, c.kRewrite, c.kRewriteClean, c.seededRewritten, ub)
	if n >= 3000 && ub > 1e-3 {
		t.Fatalf("blocking false-positive rate fails the pre-registered bound: %.6g > 1e-3", ub)
	}
	if n < 3000 && c.kBlock > 0 {
		t.Fatalf("smoke run saw %d blocking hits", c.kBlock)
	}
	// Broken variant: counting rewrites as blocks must fail the bound.
	bad := propertyRun(t, streams, 50, true)
	if WilsonUpper(bad.kBlock, bad.n, 1.96) <= 1e-3 {
		t.Fatal("the broken variant (rewrites counted as blocks) passed the bound")
	}
}
