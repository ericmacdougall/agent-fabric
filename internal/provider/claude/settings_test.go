package claude

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// goodqa#15087 AC-6 (c).

func emptyManaged(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// TestSettingSourcesAlwaysEmpty: every launch carries --setting-sources ""
// and a non-empty SettingSources refuses the launch before any argv exists.
func TestSettingSourcesAlwaysEmpty(t *testing.T) {
	g := validGrant(t)
	l := Launch{Binary: "/usr/bin/claude", Prompt: "p", ManagedRoot: emptyManaged(t)}
	argv, _, err := Argv(g, l)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "--setting-sources" {
			if argv[i+1] != "" || found {
				t.Fatalf("argv %q: want exactly one --setting-sources with an empty value", argv)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("argv %q has no --setting-sources", argv)
	}
	for _, src := range []string{"user", "project", "user,project,local", " "} {
		l.SettingSources = src
		if argv, _, err := Argv(g, l); !errors.Is(err, ErrSettings) || argv != nil {
			t.Fatalf("SettingSources %q: argv %v err %v, want ErrSettings and no argv", src, argv, err)
		}
	}
}

// hooksDir makes an owner-only hooks dir holding one owner-only script.
func hooksDir(t *testing.T) (dir, script string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "hooks")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	script = filepath.Join(dir, "guard.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir, script
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func hookSettings(cmd string) map[string]any {
	return map[string]any{"hooks": map[string]any{"PreToolUse": []any{
		map[string]any{"matcher": "Bash", "hooks": []any{map[string]any{"type": "command", "command": cmd}}},
	}}}
}

// TestSettingsAllowList: the --settings file may hold only $schema,
// permissions and single-path hooks under a hooks dir the worker cannot
// write. Each refused case refuses the launch; the accepted case launches
// with --settings.
func TestSettingsAllowList(t *testing.T) {
	g := validGrant(t)
	managed := emptyManaged(t)
	dir, script := hooksDir(t)
	outside := filepath.Join(t.TempDir(), "evil.sh")
	if err := os.WriteFile(outside, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.sh")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	me := os.Getuid()
	type tc struct {
		name     string
		settings map[string]any
		prep     func(t *testing.T) (hooks string, worker *int)
	}
	plain := func(*testing.T) (string, *int) { return dir, nil }
	cases := []tc{
		{"env block", map[string]any{"env": map[string]string{"ANTHROPIC_API_KEY": "x"}}, plain},
		{"statusLine", map[string]any{"statusLine": map[string]string{"type": "command", "command": script}}, plain},
		{"other top-level key", map[string]any{"model": "x"}, plain},
		{"hook outside FABRIC_HOOKS_DIR", hookSettings(outside), plain},
		{"hook with an argument", hookSettings(script + " --x"), plain},
		{"hook with shell syntax", hookSettings(script + ";id"), plain},
		{"hook with a relative path", hookSettings("guard.sh"), plain},
		{"hook with a dot-dot path", hookSettings(dir + "/../hooks/guard.sh"), plain},
		{"symlink in hooks dir pointing outside", hookSettings(link), plain},
		{"group-writable hooks dir", hookSettings(script), func(t *testing.T) (string, *int) {
			d, s := hooksDir(t)
			if err := os.Chmod(d, 0o770); err != nil {
				t.Fatal(err)
			}
			_ = s
			return d, nil
		}},
		{"hooks dir owned by the worker", hookSettings(script), func(*testing.T) (string, *int) { return dir, &me }},
		{"no hooks dir", hookSettings(script), func(*testing.T) (string, *int) { return "", nil }},
	}
	for _, k := range helperKeys {
		cases = append(cases, tc{"helper " + k, map[string]any{k: "/bin/true"}, plain})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hooks, worker := c.prep(t)
			settings := c.settings
			if c.name == "group-writable hooks dir" {
				settings = hookSettings(filepath.Join(hooks, "guard.sh"))
			}
			p := filepath.Join(t.TempDir(), "settings.json")
			writeJSON(t, p, settings)
			l := Launch{Binary: "/usr/bin/claude", Prompt: "p", ManagedRoot: managed, SettingsPath: p, HooksDir: hooks, WorkerUID: worker}
			if argv, _, err := Argv(g, l); !errors.Is(err, ErrSettings) || argv != nil {
				t.Fatalf("%s: argv %v err %v, want ErrSettings", c.name, argv != nil, err)
			}
		})
	}
	// Accepted: a single-path hook under an owner-only hooks dir, with a
	// worker uid that is not the owner, plus permissions and $schema.
	other := me + 1
	ok := hookSettings(script)
	ok["permissions"] = map[string]any{"deny": []string{"WebFetch"}}
	ok["$schema"] = "https://json.schemastore.org/claude-code-settings.json"
	p := filepath.Join(t.TempDir(), "settings.json")
	writeJSON(t, p, ok)
	argv, _, err := Argv(g, Launch{Binary: "/usr/bin/claude", Prompt: "p", ManagedRoot: managed, SettingsPath: p, HooksDir: dir, WorkerUID: &other})
	if err != nil || !strings.Contains(strings.Join(argv, " "), "--settings "+p) {
		t.Fatalf("accepted settings: %v", err)
	}
}

// TestManagedSettingsRefused: managed settings apply whatever
// --setting-sources says, so an env block, a helper key, an unreadable or
// an unparsable managed document refuses the launch. An empty root, and one
// with only harmless keys, is accepted.
func TestManagedSettingsRefused(t *testing.T) {
	g := validGrant(t)
	launch := func(root string) error {
		_, _, err := Argv(g, Launch{Binary: "/usr/bin/claude", Prompt: "p", ManagedRoot: root})
		return err
	}
	if err := launch(t.TempDir()); err != nil {
		t.Fatalf("empty managed root: %v", err)
	}
	if err := launch(filepath.Join(t.TempDir(), "absent")); err != nil {
		t.Fatalf("absent managed root: %v", err)
	}
	harmless := t.TempDir()
	writeJSON(t, filepath.Join(harmless, "managed-settings.json"), map[string]any{"permissions": map[string]any{"deny": []string{"WebFetch"}}})
	if err := launch(harmless); err != nil {
		t.Fatalf("harmless managed settings: %v", err)
	}
	for _, c := range []struct {
		name string
		prep func(root string)
	}{
		{"env in managed-settings.json", func(root string) {
			writeJSON(t, filepath.Join(root, "managed-settings.json"), map[string]any{"env": map[string]string{"X": "y"}})
		}},
		{"helper in a drop-in", func(root string) {
			if err := os.Mkdir(filepath.Join(root, "managed-settings.d"), 0o700); err != nil {
				t.Fatal(err)
			}
			writeJSON(t, filepath.Join(root, "managed-settings.d", "10-team.json"), map[string]any{"apiKeyHelper": "/bin/true"})
		}},
		{"unreadable managed file", func(root string) {
			f := filepath.Join(root, "managed-settings.json")
			writeJSON(t, f, map[string]any{})
			if err := os.Chmod(f, 0o000); err != nil {
				t.Fatal(err)
			}
		}},
		{"unparsable managed file", func(root string) {
			if err := os.WriteFile(filepath.Join(root, "managed-settings.json"), []byte("{not json"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"non-object drop-in", func(root string) {
			if err := os.Mkdir(filepath.Join(root, "managed-settings.d"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "managed-settings.d", "a.json"), []byte("[1]"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if os.Getuid() == 0 && c.name == "unreadable managed file" {
				t.Skip("root reads mode 000 files")
			}
			root := t.TempDir()
			c.prep(root)
			if err := launch(root); !errors.Is(err, ErrSettings) {
				t.Fatalf("%s: %v, want ErrSettings", c.name, err)
			}
		})
	}
}

// TestWriteProductionArgv: the production launch (the one fabd builds)
// carries --setting-sources "" and the hardening flags. With FABRIC_ARGV_OUT
// set it writes that argv, minus the prompt that follows -p, as JSON for
// the AC-6 (f) live tester, which inserts its own prompt after "-p".
func TestWriteProductionArgv(t *testing.T) {
	g := validGrant(t)
	l := ProductionLaunch("/usr/bin/claude", "/x/checkpoint.schema.json", "")
	l.Prompt = "PROMPT-PLACEHOLDER"
	l.ManagedRoot = emptyManaged(t)
	if out := os.Getenv("FABRIC_ARGV_OUT"); out != "" {
		l.ManagedRoot = "" // the live check runs against the real managed root
	}
	argv, _, err := Argv(g, l)
	if err != nil {
		t.Fatal(err)
	}
	if len(argv) < 3 || argv[1] != "-p" || argv[2] != "PROMPT-PLACEHOLDER" {
		t.Fatalf("argv %q does not start binary -p prompt", argv)
	}
	joined := strings.Join(argv, "\x00")
	for _, want := range []string{"--setting-sources\x00\x00", "--strict-mcp-config", "--disable-slash-commands", "--disallowedTools\x00Agent,Workflow,AskUserQuestion,WebFetch,WebSearch"} {
		if !strings.Contains(joined+"\x00", want) {
			t.Fatalf("production argv lacks %q: %q", want, argv)
		}
	}
	out := os.Getenv("FABRIC_ARGV_OUT")
	if out == "" {
		return
	}
	noPrompt := append(append([]string{}, argv[:2]...), argv[3:]...)
	b, err := json.Marshal(noPrompt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, b, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %d argv elements to %s", len(noPrompt), out)
}
