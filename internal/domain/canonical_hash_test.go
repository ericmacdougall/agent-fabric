package domain

import (
	"encoding/json"
	"testing"
)

// Red-team nit on caea50f: once CanonicalJSON refused exponents, the command
// hash fell back to raw bytes, so key order changed the hash and a command
// accepted before F3 no longer replayed. The command hash stays order-blind.
func TestCommandHashIgnoresKeyOrderWithExponent(t *testing.T) {
	a := Command{Kind: CmdSubmitEffect, Mission: "m", Request: json.RawMessage(`{"b":1,"a":1e2}`)}
	b := Command{Kind: CmdSubmitEffect, Mission: "m", Request: json.RawMessage(`{"a":1e2, "b":1}`)}
	if a.RequestHash() != b.RequestHash() {
		t.Fatalf("key order changed the command hash: %s vs %s", a.RequestHash(), b.RequestHash())
	}
	if _, err := CanonicalJSON([]byte(`{"a":1e2}`)); err == nil {
		t.Fatal("the effect canonicaliser accepted an exponent")
	}
}
