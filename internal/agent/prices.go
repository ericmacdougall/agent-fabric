package agent

import (
	"encoding/json"
	"errors"
	"fmt"
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

// ceilMul returns ceil(tokens*rate/1e6) without overflow for any int64 input
// a request can produce (rates and counts are bounded well below 2^31).
func ceilMul(tokens, rate int64) int64 {
	if tokens <= 0 || rate <= 0 {
		return 0
	}
	const lim = int64(1) << 62
	if tokens > lim/rate {
		return lim / 2 // saturate: refuses any realistic cap
	}
	return (tokens*rate + 999_999) / 1_000_000
}

// Cost is the billed estimate for the usage actually reported.
func (p Prices) Cost(u Usage) int64 {
	c := ceilMul(u.Input, *p.Input) + ceilMul(u.Output, *p.Output) +
		ceilMul(u.CacheCreate, *p.CacheWrite) + ceilMul(u.CacheRead, *p.CacheRead) +
		u.ServerToolUses**p.ServerToolUse
	if u.Input+u.CacheCreate+u.CacheRead > *p.LongContextThreshold && *p.LongContextThreshold > 0 {
		c = (c*(*p.LongContextPercent) + 99) / 100
	}
	return c
}

// WorstCase is the hold for one request: every input byte counted as a token
// at the dearer of the input and cache-write prices (a token is never less
// than one byte), plus max_tokens output (thinking included) at the output
// price, with the long-context uplift when the estimate crosses the tier.
func (p Prices) WorstCase(bodyBytes, maxTokens int) int64 {
	inRate := max(*p.Input, *p.CacheWrite, *p.CacheRead)
	c := ceilMul(int64(bodyBytes), inRate) + ceilMul(int64(maxTokens), *p.Output)
	if *p.LongContextThreshold > 0 && int64(bodyBytes) > *p.LongContextThreshold {
		c = (c*(*p.LongContextPercent) + 99) / 100
	}
	if c < 1 {
		c = 1
	}
	return c
}
