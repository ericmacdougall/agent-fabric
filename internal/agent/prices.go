package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/bits"
)

// Prices are micro-USD per million tokens (and per server-tool use). Every
// field is required: a missing field fails closed, because a silently zero
// price would turn the budget into a no-op. Values come from config only.
type Prices struct {
	Input                *int64 `json:"input_per_mtok_micros"`
	Output               *int64 `json:"output_per_mtok_micros"`
	CacheWrite           *int64 `json:"cache_write_per_mtok_micros"`
	CacheRead            *int64 `json:"cache_read_per_mtok_micros"`
	ServerToolUse        *int64 `json:"server_tool_per_use_micros"`
	LongContextThreshold *int64 `json:"long_context_threshold_tokens"`
	LongContextPercent   *int64 `json:"long_context_multiplier_percent"` // 100 = no uplift
}

var ErrPrices = errors.New("price config incomplete")

// ParsePrices decodes and validates a price config.
func ParsePrices(raw []byte) (Prices, error) {
	var p Prices
	dec := json.NewDecoder(bytesReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return Prices{}, fmt.Errorf("%w: %v", ErrPrices, err)
	}
	return p, p.Validate()
}

func (p Prices) Validate() error {
	for name, v := range map[string]*int64{
		"input_per_mtok_micros": p.Input, "output_per_mtok_micros": p.Output,
		"cache_write_per_mtok_micros": p.CacheWrite, "cache_read_per_mtok_micros": p.CacheRead,
		"server_tool_per_use_micros": p.ServerToolUse, "long_context_threshold_tokens": p.LongContextThreshold,
		"long_context_multiplier_percent": p.LongContextPercent,
	} {
		if v == nil {
			return fmt.Errorf("%w: missing %s", ErrPrices, name)
		}
		if *v < 0 {
			return fmt.Errorf("%w: negative %s", ErrPrices, name)
		}
	}
	if *p.LongContextPercent < 100 {
		return fmt.Errorf("%w: long_context_multiplier_percent below 100", ErrPrices)
	}
	return nil
}

// scaleCeil returns ceil(a*b/divisor), saturating at MaxInt64. Usage counts
// come from the provider and are untrusted, so the product is computed in
// 128 bits and never wraps.
func scaleCeil(a, b, divisor int64) int64 {
	if a <= 0 || b <= 0 || divisor <= 0 {
		return 0
	}
	hi, lo := bits.Mul64(uint64(a), uint64(b))
	if hi >= uint64(divisor) {
		return math.MaxInt64 // the quotient would not fit in 64 bits
	}
	q, rem := bits.Div64(hi, lo, uint64(divisor))
	if q > uint64(math.MaxInt64) || (q == uint64(math.MaxInt64) && rem != 0) {
		return math.MaxInt64
	}
	if rem != 0 {
		q++
	}
	return int64(q)
}

// ceilMul returns ceil(tokens*rate/1e6), saturating rather than wrapping.
func ceilMul(tokens, rate int64) int64 { return scaleCeil(tokens, rate, 1_000_000) }

// addCost adds two non-negative costs, saturating at MaxInt64.
func addCost(a, b int64) int64 {
	if a >= math.MaxInt64 || b > math.MaxInt64-a {
		return math.MaxInt64
	}
	return a + b
}

// Cost is the billed estimate for the usage actually reported.
func (p Prices) Cost(u Usage) int64 {
	c := ceilMul(u.Input, *p.Input)
	c = addCost(c, ceilMul(u.Output, *p.Output))
	c = addCost(c, ceilMul(u.CacheCreate, *p.CacheWrite))
	c = addCost(c, ceilMul(u.CacheRead, *p.CacheRead))
	c = addCost(c, scaleCeil(u.ServerToolUses, *p.ServerToolUse, 1))
	input := addCost(addCost(max(u.Input, 0), max(u.CacheCreate, 0)), max(u.CacheRead, 0))
	if *p.LongContextThreshold > 0 && input > *p.LongContextThreshold {
		c = scaleCeil(c, *p.LongContextPercent, 100)
	}
	return c
}

// WorstCase is the hold for one request: every input byte counted as a token
// at the dearer of the input and cache-write prices (a token is never less
// than one byte), plus max_tokens output (thinking included) at the output
// price, with the long-context uplift when the estimate crosses the tier.
func (p Prices) WorstCase(bodyBytes, maxTokens int) int64 {
	inRate := max(*p.Input, *p.CacheWrite, *p.CacheRead)
	c := addCost(ceilMul(int64(bodyBytes), inRate), ceilMul(int64(maxTokens), *p.Output))
	if *p.LongContextThreshold > 0 && int64(bodyBytes) > *p.LongContextThreshold {
		c = scaleCeil(c, *p.LongContextPercent, 100)
	}
	if c < 1 {
		c = 1
	}
	return c
}
