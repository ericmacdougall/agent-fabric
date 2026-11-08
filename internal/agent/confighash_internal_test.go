package agent

import "testing"

func goldenLoop(maxServerToolUses int64) *Loop {
	v := int64(1)
	p := Prices{Input: &v, Output: &v, CacheWrite: &v, CacheRead: &v, ServerToolUse: &v, LongContextThreshold: &v, LongContextPercent: &v}
	return &Loop{c: Config{Model: "golden-model", MaxTokens: 1000, System: "sys", Prompt: "prompt", Prices: p, BillingMode: BillingAPIKey,
		MaxServerToolUses: maxServerToolUses}}
}

// Resume requires an unchanged config_sha. A run started before
// MaxServerToolUses existed must keep its hash, so the zero value never
// enters it; a non-zero maximum does.
func TestConfigSHAUnchangedForZeroServerToolMaximum(t *testing.T) {
	const preField = "bbfba54aabeae7256805eef95312c32326bb3daff2797e835a7bb54266d571b6" // computed on ec6c411 loop.go
	if got := goldenLoop(0).configSHA(); got != preField {
		t.Fatalf("config_sha with no server-tool maximum = %s, want the pre-field hash %s", got, preField)
	}
	if goldenLoop(2).configSHA() == goldenLoop(0).configSHA() {
		t.Fatal("a non-zero server-tool maximum does not change config_sha")
	}
}
