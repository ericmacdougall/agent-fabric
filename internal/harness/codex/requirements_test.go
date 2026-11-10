package codex_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fabric/internal/harness"
	"fabric/internal/harness/codex"
	"fabric/internal/harness/harnesstest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const reqCanary = "PUBLIC_REQUIREMENTS_CANARY_73bd"

func TestRequirementsReducerFixedFacts(t *testing.T) {
	good := []string{`{"requirements":null}`, `{"requirements":{}}`, `{"requirements":{"modelProvider":"openai","modelProviders":{},"chatgptBaseUrl":"https://chatgpt.com/backend-api/","additionalDeveloperInstructions":"` + reqCanary + `"},"supportsIndependentSpeedModes":true}`}
	bad := []string{`{}`, `{"requirements":{"modelProvider":"other"}}`, `{"requirements":{"modelProviders":{"openai":{"secret":"` + reqCanary + `"}}}}`, `{"requirements":{"chatgptBaseUrl":"https://chatgpt.com/backend-api/?token=` + reqCanary + `"}}`, `{"requirements":{"unknown":"` + reqCanary + `"}}`, `{"requirements":{"network":{}}}`, `{"requirements":{},"requirements":null}`, `{"requirements":{"modelProviders":{"x":1,"x":2}}}`, `{"requirements":`, strings.Repeat("x", codex.RequirementsMaxBytes+1)}
	for _, s := range good {
		facts, err := codex.ReduceRequirements([]byte(s))
		if err != nil || !facts.RouteAllowed || !facts.RequiredProviderAllowed || !facts.SupportedSchema {
			t.Fatal("approved synthetic requirements refused")
		}
		b, _ := json.Marshal(facts)
		if strings.Contains(string(b), reqCanary) {
			t.Fatal("facts leaked text")
		}
	}
	for _, s := range bad {
		facts, err := codex.ReduceRequirements([]byte(s))
		if err == nil || facts.RouteAllowed || strings.Contains(err.Error(), reqCanary) {
			t.Fatal("bad requirements accepted or leaked")
		}
	}
}
func traceLeaks(evs []harness.Event) bool {
	b, _ := json.Marshal(evs)
	return strings.Contains(string(b), reqCanary)
}
func TestRequirementsWholeTransportQuarantine(t *testing.T) {
	for _, mode := range []string{"discarded", "notification", "error", "malformed", "duplicate", "oversize", "late_reply", "timeout_late"} {
		t.Run(mode, func(t *testing.T) {
			spec := startSpec(t, "")
			spec.SessionID = "0123456789abcdef0123456789abcdef"
			hooks := validHookMetadata("/home/owned", spec.Dir)
			text := "on initialize reply {}\non config/read exit 98\non config/requirements/read stderr " + reqCanary + "\n"
			switch mode {
			case "discarded":
				text += "on config/requirements/read reply {\"requirements\":{\"additionalDeveloperInstructions\":\"" + reqCanary + "\"}}\n"
			case "notification":
				text += "on config/requirements/read send {\"method\":\"warning\",\"params\":{\"message\":\"" + reqCanary + "\"}}\non config/requirements/read send {\"id\":999,\"method\":\"item/fileChange/requestApproval\",\"params\":{\"message\":\"" + reqCanary + "\"}}\non config/requirements/read reply {\"requirements\":null}\n"
			case "late_reply":
				text += "on config/requirements/read reply {\"requirements\":null}\non config/requirements/read send {\"id\":2,\"error\":{\"code\":-32000,\"message\":\"" + reqCanary + "\"}}\n"
			case "error":
				text += "on config/requirements/read error " + reqCanary + "\n"
			case "malformed":
				text += "on config/requirements/read send {" + reqCanary + "\n"
			case "duplicate":
				text += "on config/requirements/read reply {\"requirements\":null,\"requirements\":{\"additionalDeveloperInstructions\":\"" + reqCanary + "\"}}\n"
			case "oversize":
				text += "on config/requirements/read send {{pad:262200}}" + reqCanary + "\n"
			case "timeout_late":
				text += "on config/requirements/read sleep 5100\non config/requirements/read error " + reqCanary + "\n"
			}
			text += "on hooks/list reply " + string(hooks) + "\non permissionProfile/list reply {\"data\":[{\"id\":\"fabric_demo_test\",\"allowed\":true}],\"nextCursor\":null}\non thread/start exit 99\non turn/start exit 99\n"
			p := filepath.Join(t.TempDir(), "requirements.script")
			if err := os.WriteFile(p, []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
			a := &codex.Adapter{Command: []string{exe(t), "--fabric-test-script", p}, Control: &codex.Control{Profile: "fabric_demo_test", InstructionSources: []string{"/home/owned/.codex/AGENTS.md"}, Home: "/home/owned", PreflightOnly: true, CatalogOnly: true}, KillGrace: 300 * time.Millisecond, RequestTimeout: time.Second}
			rec := &harnesstest.Recorder{}
			var mc bytes.Buffer
			projected, err := codex.NewLifecycleSink(&mc, rec, spec.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			s, err := a.Start(context.Background(), spec, projected)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			f, err := s.Wait(ctx)
			if err != nil {
				_ = s.Stop(context.Background())
				t.Fatal(err)
			}
			want := harness.OutcomeFailed
			if mode == "discarded" || mode == "notification" || mode == "late_reply" {
				want = harness.OutcomeStopped
			}
			if f.Outcome != want {
				t.Fatalf("unexpected fixed outcome %s", f.Outcome)
			}
			evs := rec.Snapshot()
			if strings.Contains(mc.String(), reqCanary) {
				t.Fatal("requirements data leaked to MC lifecycle output")
			}
			if traceLeaks(evs) {
				t.Fatal("private transport data reached event trace")
			}
			for _, e := range evs {
				if e.Kind == harness.KindTurn || e.Kind == harness.KindMessage || e.Kind == harness.KindToolCall {
					t.Fatal("sensitive phase dispatched a model/tool/message")
				}
			}
		})
	}
}
func TestBrokenRequirementsForwarderIsDetected(t *testing.T) {
	rec := &harnesstest.Recorder{}
	em, _ := harness.NewEmitter("codex", "owned", rec, harness.PatternRedactor())
	_ = em.Unknown(context.Background(), "broken-forwarder", reqCanary)
	if !traceLeaks(rec.Snapshot()) {
		t.Fatal("public canary control failed to detect deliberately broken forwarding")
	}
}
