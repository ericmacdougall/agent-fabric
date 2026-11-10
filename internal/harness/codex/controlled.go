package codex

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// Control is a controller-owned policy, never model-supplied JSON. A profile
// alone is insufficient: config and the thread's effective roots/instructions
// are checked before dispatch. PreflightOnly never sends turn/start.
// Runtime source/backend/hook and OS denial proofs remain an external gate.
type Control struct {
	Profile            string
	InstructionSources []string
	PreflightOnly      bool
}

// FileEditSettings is the minimum explicit configuration ceiling. Inherited
// registrations and shell set maps require separate checks below; empty maps
// are not assumed to replace inherited TOML maps.
func FileEditSettings() map[string]any {
	return map[string]any{
		"approval_policy": "never", "web_search": "disabled", "allow_login_shell": false,
		"features": map[string]any{
			"shell_tool": false, "unified_exec": false, "shell_snapshot": false, "multi_agent": false, "multi_agent_v2": false,
			"apps": false, "plugins": false, "remote_plugin": false, "plugin_discovery_prefetch": false, "plugin_sharing": false,
			"memories": false, "external_agent_memory_import": false, "deferred_executor": false, "hooks": true,
			"browser_use": false, "browser_use_external": false, "browser_use_full_cdp_access": false, "computer_use": false, "image_generation": false, "view_image": false, "in_app_browser": false, "code_mode": false, "code_mode_host": false, "code_mode_interrupt": false, "code_mode_only": false, "code_mode_prewarm": false, "code_mode_tool_search": false, "multi_agent_v2_dynamic_tools": false, "multi_env_copy_file": false, "subagent_thread_environments": false, "runtime_agent_instances": false, "skill_mcp_dependency_install": false, "skill_search": false, "tool_suggest": false, "workspace_dependencies": false, "request_permissions_tool": false, "executor_capability_discovery": false, "executor_capability_discovery_v2": false, "incremental_exec_tools": false, "artifact": false,
		},
		"memories":                 map[string]any{"use_memories": false, "generate_memories": false},
		"shell_environment_policy": map[string]any{"inherit": "none", "experimental_use_profile": false, "ignore_default_excludes": false, "set": map[string]any{}},
	}
}

// CheckFileEditConfig reports fixed categories only: values may contain
// credentials or historical private context. Missing readback is a refusal.
func CheckFileEditConfig(config map[string]any) error {
	var check func(map[string]any, map[string]any) bool
	check = func(got, want map[string]any) bool {
		for key, v := range want {
			if nested, ok := v.(map[string]any); ok {
				child, ok := got[key].(map[string]any)
				if !ok {
					return false
				}
				if len(nested) == 0 {
					if len(child) != 0 {
						return false
					}
				} else if !check(child, nested) {
					return false
				}
			} else if !reflect.DeepEqual(got[key], v) {
				return false
			}
		}
		return true
	}
	if !check(config, FileEditSettings()) {
		return errors.New("codex: controlled configuration mismatch")
	}
	for _, key := range []string{"developer_instructions", "instructions", "model_instructions_file", "experimental_instructions_file", "compact_prompt", "notify"} {
		v := config[key]
		if v != nil && v != "" {
			return errors.New("codex: unapproved instruction or startup source")
		}
	}
	if policy, ok := config["shell_environment_policy"].(map[string]any); ok {
		for k := range policy {
			switch k {
			case "inherit", "experimental_use_profile", "ignore_default_excludes", "set":
			default:
				return errors.New("codex: unapproved shell environment source")
			}
		}
	}
	for _, key := range []string{"mcp_servers", "plugins"} {
		if config[key] == nil {
			continue
		}
		entries, ok := config[key].(map[string]any)
		if !ok {
			return errors.New("codex: unverified integration configuration")
		}
		for _, v := range entries {
			entry, ok := v.(map[string]any)
			if !ok || entry["enabled"] != false {
				return errors.New("codex: integration still enabled")
			}
		}
	}
	return nil
}

func (c *Control) checkThread(raw json.RawMessage, dir string) error {
	var reply struct {
		Cwd      string `json:"cwd"`
		Approval string `json:"approvalPolicy"`
		Profile  struct {
			ID string `json:"id"`
		} `json:"activePermissionProfile"`
		Roots        []string `json:"runtimeWorkspaceRoots"`
		Instructions []string `json:"instructionSources"`
	}
	if json.Unmarshal(raw, &reply) != nil || reply.Cwd != dir || reply.Approval != "never" || reply.Profile.ID != c.Profile || len(reply.Roots) != 1 || reply.Roots[0] != dir {
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

func (c *Control) validate() error {
	if c.Profile == "" || len(c.Profile) > 80 || strings.ContainsAny(c.Profile, " ./:\\\x00\n") {
		return errors.New("codex: invalid controlled profile")
	}
	if len(c.InstructionSources) == 0 {
		return errors.New("codex: required instruction sources missing")
	}
	return nil
}

func (s *session) controlledConfig(raw json.RawMessage) error {
	var result struct {
		Config map[string]any `json:"config"`
	}
	if json.Unmarshal(raw, &result) != nil || result.Config == nil {
		return errors.New("codex: configuration readback unavailable")
	}
	if err := CheckFileEditConfig(result.Config); err != nil {
		return err
	}
	profiles, ok := result.Config["permissions"].(map[string]any)
	if !ok || !reflect.DeepEqual(profiles[s.control.Profile], FileEditPermissions(s.spec.Dir)) {
		return errors.New("codex: named permission profile mismatch")
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
		params["dynamicTools"] = []any{}
	} else {
		params["sandbox"] = s.opts.sandbox
		if s.opts.resume != "" {
			method = "thread/resume"
			params["threadId"] = s.opts.resume
		}
	}
	if s.spec.Model != "" {
		params["model"] = s.spec.Model
	}
	if err := s.request(method, params, "thread"); err != nil {
		s.fail(fmt.Sprintf("cannot send %s", method))
	}
}

// FileEditPermissions is the exact invocation-only profile checked on readback.
func FileEditPermissions(dir string) map[string]any {
	return map[string]any{"extends": ":workspace", "filesystem": map[string]any{":root": "deny", ":minimal": "read", ":tmpdir": "deny", ":slash_tmp": "deny", dir: "write"}, "network": map[string]any{"enabled": false}}
}
