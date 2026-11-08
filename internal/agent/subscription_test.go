package agent_test

import (
	"errors"
	"strings"
	"testing"

	"fabric/internal/agent"
)

// goodqa#15087 NEG-11 (AC-5): a loop configured with subscription billing
// is refused, and the message does not promise that subscription billing
// arrives later. An API-key loop with the same config is accepted
// (control).
func TestSubscriptionBillingRefused(t *testing.T) {
	r := newRig(t, endStep("x"))
	cfg := agent.Config{DB: r.db, Ledger: &agent.Ledger{}, Provider: r.prov, Redact: canaryRedactor{}, Model: "m", MaxTokens: 1, MaxTurns: 1,
		Wall: 1, Prices: testPrices(), BillingMode: agent.BillingSubscription}
	_, err := agent.New(cfg)
	if !errors.Is(err, agent.ErrSubscriptionBilling) {
		t.Fatalf("subscription billing: %v, want ErrSubscriptionBilling", err)
	}
	msg := strings.ToLower(err.Error())
	for _, promise := range []string{"arrives", "slice", "later", "coming", "soon", "not yet"} {
		if strings.Contains(msg, promise) {
			t.Fatalf("refusal %q promises subscription billing (%q)", err, promise)
		}
	}
	cfg.BillingMode = agent.BillingAPIKey
	if _, err := agent.New(cfg); err != nil {
		t.Fatalf("api-key loop with the same config: %v", err)
	}
}
