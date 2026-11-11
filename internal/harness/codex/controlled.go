package codex

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Control is a controller-owned policy, never model-supplied JSON. A profile
// alone is insufficient. The metadata check never requests full config, and
// the thread check binds actual roots, instructions and provider.
// PreflightOnly never sends turn/start; CatalogOnly never starts a thread.
// Runtime source/backend/hook and OS denial proofs remain an external gate.
type Control struct {
	Profile            string
	InstructionSources []string
	PreflightOnly      bool
	CatalogOnly        bool
	Home               string
	// ProxyURL binds literal native proxy variables; the launcher must apply
	// the independently tested forced-egress OS boundary as well.
	ProxyURL string
}

func (c *Control) checkProxyEnv(env map[string]string) error {
	if c.ProxyURL == "" {
		return nil
	}
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		if env[k] != c.ProxyURL {
			return errors.New("codex: incomplete owned proxy environment")
		}
	}
	for _, k := range []string{"NO_PROXY", "no_proxy"} {
		v, ok := env[k]
		if !ok || v != "" {
			return errors.New("codex: proxy bypass refused")
		}
	}
	return nil
}

// FileEditSettings is the minimum explicit configuration ceiling. Inherited
// registrations and shell set maps require separate checks below; empty maps
// are not assumed to replace inherited TOML maps.
func FileEditSettings() map[string]any {
	return map[string]any{
		"approval_policy": "never", "web_search": "disabled", "allow_login_shell": false,
		"check_for_update_on_startup": false,
		"model_provider":              "openai", "forced_login_method": "chatgpt", "notify": []any{},
		"openai_base_url": InferenceBootstrapURL, "chatgpt_base_url": ChatGPTBootstrapURL,
		"analytics": map[string]any{"enabled": false},
		"model":     DemoModel, "model_reasoning_effort": "low",
		"skills":    map[string]any{"include_instructions": false, "bundled": map[string]any{"enabled": false}},
		"code_mode": map[string]any{"disable_in_process_fallback": false},
		"features": map[string]any{
			"shell_tool": false, "unified_exec": false, "shell_snapshot": false, "multi_agent": false, "multi_agent_v2": false,
			"apps": false, "plugins": false, "remote_plugin": false, "plugin_discovery_prefetch": false, "plugin_sharing": false,
			"memories": false, "external_agent_memory_import": false, "deferred_executor": false, "hooks": true,
			"browser_use": false, "browser_use_external": false, "browser_use_full_cdp_access": false, "computer_use": false, "image_generation": false, "view_image": false, "in_app_browser": false, "code_mode": false, "code_mode_host": false, "code_mode_interrupt": false, "code_mode_only": false, "code_mode_prewarm": false, "code_mode_tool_search": false, "multi_agent_v2_dynamic_tools": false, "multi_env_copy_file": false, "subagent_thread_environments": false, "runtime_agent_instances": false, "skill_mcp_dependency_install": false, "skill_search": false, "tool_suggest": false, "workspace_dependencies": false, "request_permissions_tool": false, "executor_capability_discovery": false, "executor_capability_discovery_v2": false, "incremental_exec_tools": false, "artifact": false, "skip_host_skill_discovery": true, "omit_skill_descriptions": true, "unbounded_connection_retries": false, "cli_daybreak": false, "goals": false, "daemon_auto_start": false,
		},
		"memories":                 map[string]any{"use_memories": false, "generate_memories": false},
		"shell_environment_policy": map[string]any{"inherit": "none", "experimental_use_profile": false, "ignore_default_excludes": false, "set": map[string]any{}},
	}
}

// CheckFileEditFeatures validates the pinned client's metadata-only
// `features list` output. It never requests or accepts a full config dump.
func CheckFileEditFeatures(raw []byte) error {
	if len(raw) == 0 || len(raw) > 64<<10 {
		return errors.New("codex: feature metadata unavailable")
	}
	want := FileEditSettings()["features"].(map[string]any)
	seen := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		expected, ok := want[f[0]]
		if !ok {
			continue
		}
		if seen[f[0]] || len(f) < 3 || (f[len(f)-1] != "true" && f[len(f)-1] != "false") {
			return errors.New("codex: malformed feature metadata")
		}
		seen[f[0]] = true
		// This version forces unified_exec on without a managed requirement.
		// shell_tool=false independently prevents both exec handler variants.
		if f[0] != "unified_exec" && (f[len(f)-1] == "true") != expected.(bool) {
			return fmt.Errorf("codex: feature ceiling mismatch: %s", f[0])
		}
	}
	for name := range want {
		if !seen[name] {
			return errors.New("codex: required feature metadata missing")
		}
	}
	return nil
}

func (c *Control) checkThread(raw json.RawMessage, dir string) error {
	var reply struct {
		Provider string `json:"modelProvider"`
		Model    string `json:"model"`
		Cwd      string `json:"cwd"`
		Approval string `json:"approvalPolicy"`
		Profile  struct {
			ID string `json:"id"`
		} `json:"activePermissionProfile"`
		Roots        []string `json:"runtimeWorkspaceRoots"`
		Instructions []string `json:"instructionSources"`
	}
	if json.Unmarshal(raw, &reply) != nil || reply.Cwd != dir || reply.Provider != "openai" || reply.Model != DemoModel || reply.Approval != "never" || reply.Profile.ID != c.Profile || len(reply.Roots) != 1 || reply.Roots[0] != dir {
		return errors.New("codex: effective thread permissions mismatch")
	}
	got := append([]string(nil), reply.Instructions...)
	want := append([]string(nil), c.InstructionSources...)
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		return errors.New("codex: effective instruction sources mismatch")
	}
	return nil
}

var profileID = regexp.MustCompile(`^[a-z][a-z0-9_]{0,79}$`)

func (c *Control) validate() error {
	if c.ProxyURL != "" {
		u, e := url.Parse(c.ProxyURL)
		if e != nil || u.Scheme != "http" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("codex: invalid owned proxy")
		}
		h, p, e := net.SplitHostPort(u.Host)
		n, ne := strconv.ParseUint(p, 10, 16)
		if e != nil || ne != nil || h != "127.0.0.1" || n == 0 {
			return errors.New("codex: invalid owned proxy")
		}
	}
	if !profileID.MatchString(c.Profile) {
		return errors.New("codex: invalid controlled profile")
	}
	if c.Home == "" || !filepath.IsAbs(c.Home) {
		return errors.New("codex: controlled home missing")
	}
	if len(c.InstructionSources) == 0 {
		return errors.New("codex: required instruction sources missing")
	}
	return nil
}

func (s *session) controlledProfiles(raw json.RawMessage) error {
	var result struct {
		Data []struct {
			ID      string `json:"id"`
			Allowed bool   `json:"allowed"`
		} `json:"data"`
		NextCursor *string `json:"nextCursor"`
	}
	if json.Unmarshal(raw, &result) != nil || result.NextCursor != nil {
		return errors.New("codex: permission profile metadata unavailable")
	}
	n := 0
	for _, p := range result.Data {
		if p.ID == s.control.Profile && p.Allowed {
			n++
		}
	}
	if n != 1 {
		return errors.New("codex: named permission profile unavailable")
	}
	return nil
}

func (s *session) startThread() {
	s.phase = phThread
	params := map[string]any{"cwd": s.spec.Dir, "approvalPolicy": s.opts.approval}
	method := "thread/start"
	if s.control != nil {
		params["permissions"] = s.control.Profile
		params["ephemeral"] = true
		params["allowProviderModelFallback"] = false
		params["modelProvider"] = "openai"
		params["model"] = DemoModel
		params["runtimeWorkspaceRoots"] = []string{s.spec.Dir}
		params["dynamicTools"] = []any{}
	} else {
		params["sandbox"] = s.opts.sandbox
		if s.opts.resume != "" {
			method = "thread/resume"
			params["threadId"] = s.opts.resume
		}
	}
	if s.spec.Model != "" && s.control == nil {
		params["model"] = s.spec.Model
	}
	if err := s.request(method, params, "thread"); err != nil {
		s.fail(fmt.Sprintf("cannot send %s", method))
	}
}

// FileEditPermissions is the exact invocation-only profile checked on readback.
const DemoArtifact = "demo.txt"
const DemoModel = "gpt-6-luna"
const DemoPrompt = "Use only apply_patch to create demo.txt containing exactly fabric followed by one newline. Do not run commands, access any other file, use any other tool, or create additional files. Complete the single file edit and stop."

func FileEditPermissions(dir string) map[string]any {
	return map[string]any{"filesystem": map[string]any{":root": "deny", ":minimal": "read", ":tmpdir": "deny", ":slash_tmp": "deny", dir: "read", filepath.Join(dir, DemoArtifact): "write"}, "network": map[string]any{"enabled": false}}
}

func (s *session) controlledModels(raw json.RawMessage) error {
	var r struct {
		Data []struct {
			Model                     string
			Hidden                    bool
			SupportedReasoningEfforts []struct {
				ReasoningEffort string `json:"reasoningEffort"`
			}
		}
		NextCursor *string
	}
	if json.Unmarshal(raw, &r) != nil || r.NextCursor != nil {
		return errors.New("codex: model metadata unavailable")
	}
	n := 0
	for _, m := range r.Data {
		if m.Model == DemoModel && !m.Hidden {
			for _, e := range m.SupportedReasoningEfforts {
				if e.ReasoningEffort == "low" {
					n++
				}
			}
		}
	}
	if n != 1 {
		return errors.New("codex: selected subscription model absent from catalog")
	}
	return nil
}
