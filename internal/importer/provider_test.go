package importer

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"fabric/internal/domain"
	"fabric/internal/storetest"
)

// goodqa#15087 AC-1, NEG-1: an import row is a claude-code mission. An
// absent provider or claude-code is accepted; anything else is refused and
// names the row, and so is a re-import over an anthropic-api mission.
func TestImportProvider(t *testing.T) {
	ctx := context.Background()
	slots := func(t *testing.T, row string) SlotsFile {
		t.Helper()
		var sf SlotsFile
		if err := json.Unmarshal([]byte(`{"slots":[`+row+`]}`), &sf); err != nil {
			t.Fatal(err)
		}
		return sf
	}
	o := fixture(t)
	for _, c := range []struct{ what, row string }{
		{"absent", `{"slot":1,"name":"a","ticket":"GoodQA goodqa#1"}`},
		{"claude-code", `{"slot":1,"name":"a","ticket":"GoodQA goodqa#1","provider":"claude-code"}`},
	} {
		st := storetest.Open(t)
		if _, err := Run(ctx, st, nil, o, slots(t, c.row), QueueFile{}); err != nil {
			t.Fatalf("%s provider: %v", c.what, err)
		}
		var p string
		if err := st.Pool().QueryRow(ctx, "SELECT provider FROM missions WHERE id = 'm_slot1'").Scan(&p); err != nil || p != "claude-code" {
			t.Fatalf("%s provider stored as %q (%v), want claude-code", c.what, p, err)
		}
	}
	for _, c := range []struct{ what, row string }{
		{"anthropic-api", `{"slot":1,"name":"a","ticket":"GoodQA goodqa#1","provider":"anthropic-api"}`},
		{"unknown", `{"slot":1,"name":"a","ticket":"GoodQA goodqa#1","provider":"claude.ai"}`},
		{"empty", `{"slot":1,"name":"a","ticket":"GoodQA goodqa#1","provider":""}`},
	} {
		st := storetest.Open(t)
		_, err := Run(ctx, st, nil, o, slots(t, c.row), QueueFile{})
		if !errors.Is(err, domain.ErrInvalidProvider) || !strings.Contains(err.Error(), "m_slot1") {
			t.Fatalf("%s provider: %v, want ErrInvalidProvider naming m_slot1", c.what, err)
		}
		var n int
		if err := st.Pool().QueryRow(ctx, "SELECT count(*) FROM missions").Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s provider left %d missions (%v)", c.what, n, err)
		}
	}
	// A queued object row is held to the same rule.
	st := storetest.Open(t)
	var sf SlotsFile
	if err := json.Unmarshal([]byte(`{"queued_in_order":["plain item goodqa#2",{"name":"q","ticket":"GoodQA goodqa#3","provider":"anthropic-api"}]}`), &sf); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(ctx, st, nil, o, sf, QueueFile{}); !errors.Is(err, domain.ErrInvalidProvider) || !strings.Contains(err.Error(), "m_q02") {
		t.Fatalf("queued anthropic-api row: %v, want ErrInvalidProvider naming m_q02", err)
	}
	// A re-import over an existing anthropic-api mission of the same id.
	st = storetest.Open(t)
	if err := st.CreateMission(ctx, "m_slot1", domain.ProviderAnthropicAPI, "GoodQA goodqa#1", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(ctx, st, nil, o, slots(t, `{"slot":1,"name":"a","ticket":"GoodQA goodqa#1"}`), QueueFile{}); !errors.Is(err, domain.ErrInvalidProvider) {
		t.Fatalf("re-import over an anthropic-api mission: %v, want ErrInvalidProvider", err)
	}
}
