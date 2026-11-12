package native_test

import (
	"encoding/json"
	"strings"
	"testing"

	"fabric/internal/native"
)

func TestGrantCompaction(t *testing.T) {
	base := func() native.Grant { return newEnv(t).g }
	bad := map[string]func(g *native.Grant){
		"server without an output cap": func(g *native.Grant) { g.Compaction = &native.CompactionGrant{Mode: "server"} },
		"server trigger below 50000": func(g *native.Grant) {
			g.Compaction = &native.CompactionGrant{Mode: "server", OutputCap: 1000, TriggerTokens: 49_999}
		},
		"client without a window": func(g *native.Grant) { g.Compaction = &native.CompactionGrant{Mode: "client"} },
		"client trigger below 8000": func(g *native.Grant) {
			g.Compaction = &native.CompactionGrant{Mode: "client", WindowTokens: 100_000, TriggerTokens: 7_999}
		},
		"client trigger above the window": func(g *native.Grant) {
			g.Compaction = &native.CompactionGrant{Mode: "client", WindowTokens: 100_000, TriggerTokens: 100_001}
		},
		"unknown mode": func(g *native.Grant) { g.Compaction = &native.CompactionGrant{Mode: "auto"} },
		"clear_tool_uses through extra": func(g *native.Grant) {
			g.Compaction = &native.CompactionGrant{Mode: "server", OutputCap: 1000}
			g.Extra = map[string]json.RawMessage{"context_management": json.RawMessage(`{"edits":[{"type":"clear_tool_uses_20250919"}]}`)}
		},
		"context_management with no compaction": func(g *native.Grant) {
			g.Extra = map[string]json.RawMessage{"context_management": json.RawMessage(`{"edits":[{"type":"compact_20260112"}]}`)}
		},
		"beta outside the allowlist": func(g *native.Grant) {
			g.Compaction = &native.CompactionGrant{Mode: "server", OutputCap: 1000}
			g.Betas = []string{"context-1m-2025-08-07"}
		},
		"beta without server mode": func(g *native.Grant) { g.Betas = []string{"compact-2026-01-12"} },
	}
	for name, mod := range bad {
		g := base()
		mod(&g)
		if err := g.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for name, c := range map[string]*native.CompactionGrant{
		"server":      {Mode: "server", OutputCap: 2000, TriggerTokens: 80_000},
		"server dflt": {Mode: "server", OutputCap: 2000},
		"client":      {Mode: "client", WindowTokens: 200_000, TriggerTokens: 150_000},
		"client dflt": {Mode: "client", WindowTokens: 200_000},
	} {
		g := base()
		g.Compaction = c
		if err := g.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// the server request carries only the compact edit, the beta, and its trigger
	g := base()
	g.Compaction = &native.CompactionGrant{Mode: "server", OutputCap: 2000, TriggerTokens: 80_000}
	extra, betas, err := native.CompactionExtraForTest(g)
	if err != nil || len(betas) != 1 || betas[0] != "compact-2026-01-12" {
		t.Fatalf("%v %v", betas, err)
	}
	cm := string(extra["context_management"])
	if strings.Count(cm, `"type":"compact_20260112"`) != 1 || strings.Contains(cm, "clear_") || !strings.Contains(cm, `"value":80000`) || !strings.Contains(cm, "Do not call any tools") {
		t.Fatalf("context_management %s", cm)
	}
	// client mode sends nothing to the API
	g.Compaction = &native.CompactionGrant{Mode: "client", WindowTokens: 100_000}
	if extra, betas, _ := native.CompactionExtraForTest(g); len(betas) != 0 || extra["context_management"] != nil {
		t.Fatal("client mode added API parameters")
	}
	// a resume under a different compaction setting is refused: the pin differs
	a, b := base(), base()
	b.Compaction = &native.CompactionGrant{Mode: "client", WindowTokens: 100_000}
	if native.GrantPinForTest(a, false) == native.GrantPinForTest(b, false) {
		t.Fatal("the grant pin ignores compaction")
	}
}
