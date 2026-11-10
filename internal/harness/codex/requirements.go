package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

const RequirementsMaxBytes = 256 << 10
const InferenceBootstrapURL = "https://chatgpt.com/backend-api/codex"
const ChatGPTBootstrapURL = "https://chatgpt.com/backend-api/"

// RequirementsFacts contains no arbitrary input strings, hashes or values.
// Raw requirements are private app-server/native-client data only. They never
// enter harness events, diagnostics, traces, MC, receipts or a model prompt.
type RequirementsFacts struct {
	SupportedSchema         bool
	RouteAllowed            bool
	RequiredProviderAllowed bool
}

var errRequirements = errors.New("codex: requirements refused")

// privateJSON rejects duplicate keys at every depth, trailing data and bounded
// structural exhaustion. Errors intentionally contain no parser diagnostics.
func privateJSON(raw []byte) (any, error) {
	if len(raw) == 0 || len(raw) > RequirementsMaxBytes {
		return nil, errRequirements
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	nodes := 0
	var value func(int) (any, error)
	value = func(depth int) (any, error) {
		nodes++
		if depth > 64 || nodes > 65536 {
			return nil, errRequirements
		}
		t, e := d.Token()
		if e != nil {
			return nil, errRequirements
		}
		delim, ok := t.(json.Delim)
		if !ok {
			return t, nil
		}
		switch delim {
		case '{':
			m := map[string]any{}
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return nil, errRequirements
				}
				s, ok := k.(string)
				if !ok {
					return nil, errRequirements
				}
				if _, exists := m[s]; exists {
					return nil, errRequirements
				}
				v, e := value(depth + 1)
				if e != nil {
					return nil, e
				}
				m[s] = v
			}
			t, e = d.Token()
			if e != nil || t != json.Delim('}') {
				return nil, errRequirements
			}
			return m, nil
		case '[':
			var a []any
			for d.More() {
				v, e := value(depth + 1)
				if e != nil {
					return nil, e
				}
				a = append(a, v)
			}
			t, e = d.Token()
			if e != nil || t != json.Delim(']') {
				return nil, errRequirements
			}
			return a, nil
		default:
			return nil, errRequirements
		}
	}
	v, e := value(0)
	if e != nil {
		return nil, e
	}
	if _, e = d.Token(); e != io.EOF {
		return nil, errRequirements
	}
	return v, nil
}

// ReduceRequirements is deliberately narrow: unknown/non-null constraints
// refuse rather than guessing their precedence. Managed policy text is retained
// by the first-party server; this reducer discards it without echoing/hashing.
func ReduceRequirements(raw []byte) (RequirementsFacts, error) {
	no := RequirementsFacts{}
	v, err := privateJSON(raw)
	if err != nil {
		return no, errRequirements
	}
	root, ok := v.(map[string]any)
	if !ok {
		return no, errRequirements
	}
	for k, val := range root {
		switch k {
		case "requirements":
		case "supportsIndependentSpeedModes":
			if val != nil {
				if _, ok := val.(bool); !ok {
					return no, errRequirements
				}
			}
		default:
			return no, errRequirements
		}
	}
	req, exists := root["requirements"]
	if !exists {
		return no, errRequirements
	}
	if req == nil {
		return RequirementsFacts{true, true, true}, nil
	}
	m, ok := req.(map[string]any)
	if !ok {
		return no, errRequirements
	}
	knownNull := map[string]bool{"allowAppshots": true, "allowBrowserAndComputerUse": true, "allowLoginShell": true, "allowManagedHooksOnly": true, "allowRemoteControl": true, "allowedApprovalPolicies": true, "allowedApprovalsReviewers": true, "allowedLoginMethods": true, "allowedPermissionProfiles": true, "allowedSandboxModes": true, "allowedWebSearchModes": true, "allowedWindowsSandboxImplementations": true, "application": true, "autoReview": true, "browserUse": true, "checkForUpdateOnStartup": true, "cliAuthCredentialsStore": true, "computerUse": true, "defaultPermissions": true, "enforceResidency": true, "featureRequirements": true, "feedback": true, "hooks": true, "inAppBrowser": true, "logDir": true, "modelCatalogJson": true, "models": true, "network": true, "otel": true, "sqliteHome": true}
	for k, val := range m {
		switch k {
		case "modelProvider":
			if val != nil && val != "openai" {
				return no, errRequirements
			}
		case "chatgptBaseUrl":
			if val != nil && val != ChatGPTBootstrapURL {
				return no, errRequirements
			}
		case "modelProviders":
			if val != nil {
				providers, ok := val.(map[string]any)
				if !ok || len(providers) > 0 {
					return no, errRequirements
				}
			}
		case "additionalDeveloperInstructions":
			if val != nil {
				if _, ok := val.(string); !ok {
					return no, errRequirements
				}
			}
		default:
			if !knownNull[k] || val != nil {
				return no, errRequirements
			}
		}
	}
	return RequirementsFacts{true, true, true}, nil
}

// requirementsFrame quarantines the ENTIRE sensitive phase. Unexpected
// notifications/requests/replies are discarded privately, never dispatched to
// normal handlers. Request failures end the session; late queued data cannot
// reach a journal because the actor closes before draining raw frames.
func (s *session) requirementsFrame(l line) {
	if l.over || l.size > RequirementsMaxBytes {
		s.fail("codex: requirements refused")
		return
	}
	v, err := privateJSON(l.data)
	if err != nil {
		s.fail("codex: requirements refused")
		return
	}
	envelope, ok := v.(map[string]any)
	if !ok {
		s.fail("codex: requirements refused")
		return
	}
	number, ok := envelope["id"].(json.Number)
	if !ok || number.String() != s.requirementsID {
		return
	}
	if envelope["method"] != nil || envelope["error"] != nil {
		s.fail("codex: requirements refused")
		return
	}
	for k := range envelope {
		if k != "id" && k != "result" && k != "jsonrpc" {
			s.fail("codex: requirements refused")
			return
		}
	}
	payload, ok := envelope["result"]
	if !ok {
		s.fail("codex: requirements refused")
		return
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		s.fail("codex: requirements refused")
		return
	}
	facts, err := ReduceRequirements(raw)
	if err != nil || !facts.SupportedSchema || !facts.RouteAllowed || !facts.RequiredProviderAllowed {
		s.fail("codex: requirements refused")
		return
	}
	delete(s.pend, s.nextReq)
	s.disarm()
	s.requirementsPhase = false
	if err := s.request("hooks/list", map[string]any{"cwds": []string{s.spec.Dir}}, "controlled_hooks"); err != nil {
		s.fail("codex: hook metadata unavailable")
	}
}
