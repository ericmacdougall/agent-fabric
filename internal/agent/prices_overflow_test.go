package agent_test

import (
	"math"
	"math/big"
	"testing"

	"fabric/internal/agent"
)

// Usage counters come from the provider, so they are untrusted. No reported
// count may wrap the cost negative or below an honest smaller report.
func TestPriceArithmeticSaturates(t *testing.T) {
	p := agent.Prices{
		Input: i64(3), Output: i64(15), CacheWrite: i64(4), CacheRead: i64(1),
		ServerToolUse: i64(10_000), LongContextThreshold: i64(200_000),
		LongContextPercent: i64(200),
	}
	if got := p.Cost(agent.Usage{ServerToolUses: math.MaxInt64 / 2}); got != math.MaxInt64 {
		t.Errorf("server tool uses: Cost = %d, want saturation at MaxInt64", got)
	}
	// Large but representable totals must be exact: ceil(n*rate/1e6) per
	// counter, summed, then the 200% uplift once the input sum (which itself
	// would wrap in int64) crosses the threshold.
	ceil := func(n, rate int64) *big.Int {
		v := new(big.Int).Mul(big.NewInt(n), big.NewInt(rate))
		v.Add(v, big.NewInt(999_999))
		return v.Div(v, big.NewInt(1_000_000))
	}
	for name, tc := range map[string]struct {
		u    agent.Usage
		want *big.Int
	}{
		"token sum": {agent.Usage{Input: math.MaxInt64, Output: math.MaxInt64},
			new(big.Int).Add(ceil(math.MaxInt64, 3), ceil(math.MaxInt64, 15))},
		"long-context sum": {agent.Usage{Input: math.MaxInt64 / 2, CacheRead: math.MaxInt64 / 2, CacheCreate: math.MaxInt64 / 2},
			new(big.Int).Add(new(big.Int).Add(ceil(math.MaxInt64/2, 3), ceil(math.MaxInt64/2, 1)), ceil(math.MaxInt64/2, 4))},
	} {
		want := new(big.Int).Mul(tc.want, big.NewInt(2)) // both cross the threshold
		if got := p.Cost(tc.u); !want.IsInt64() || got != want.Int64() {
			t.Errorf("%s: Cost = %d, want %s", name, got, want)
		}
	}

	huge := agent.Prices{
		Input: i64(math.MaxInt64), Output: i64(math.MaxInt64),
		CacheWrite: i64(math.MaxInt64), CacheRead: i64(math.MaxInt64),
		ServerToolUse: i64(math.MaxInt64), LongContextThreshold: i64(1),
		LongContextPercent: i64(math.MaxInt64),
	}
	if got := huge.WorstCase(math.MaxInt, math.MaxInt, math.MaxInt64); got != math.MaxInt64 {
		t.Errorf("overflowing WorstCase = %d, want MaxInt64", got)
	}

	// Away from saturation the exact ceiling semantics hold, uplift included:
	// ceil(2_000_001*1/1e6) = 3, then ceil(3*101/100) = 4.
	q := agent.Prices{
		Input: i64(1), Output: i64(1), CacheWrite: i64(1), CacheRead: i64(1),
		ServerToolUse: i64(1), LongContextThreshold: i64(1), LongContextPercent: i64(101),
	}
	if got := q.Cost(agent.Usage{Input: 2_000_001}); got != 4 {
		t.Errorf("ceiling Cost with uplift = %d, want 4", got)
	}
	if got := q.Cost(agent.Usage{ServerToolUses: 7}); got != 7 {
		t.Errorf("server tool Cost = %d, want 7", got)
	}
}
