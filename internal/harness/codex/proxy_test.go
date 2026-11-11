package codex

import "testing"

func TestControlledProxyIsLiteralAndComplete(t *testing.T) {
	c := Control{Profile: "fabric_demo_test", InstructionSources: []string{"required"}, Home: "/owned", ProxyURL: "http://127.0.0.1:12345"}
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		env[k] = c.ProxyURL
	}
	env["NO_PROXY"] = ""
	env["no_proxy"] = ""
	if err := c.checkProxyEnv(env); err != nil {
		t.Fatal(err)
	}
	for k := range env {
		old := env[k]
		delete(env, k)
		if c.checkProxyEnv(env) == nil {
			t.Fatal("missing proxy control accepted", k)
		}
		env[k] = old
	}
	env["NO_PROXY"] = "localhost"
	if c.checkProxyEnv(env) == nil {
		t.Fatal("bypass accepted")
	}
	for _, bad := range []string{"http://localhost:12345", "https://127.0.0.1:12345", "http://127.0.0.1:0", "http://127.0.0.1:12345/path", "http://public@127.0.0.1:12345", "http://127.0.0.1:12345?x=1"} {
		c.ProxyURL = bad
		if c.validate() == nil {
			t.Fatal("unsafe proxy", bad)
		}
	}
}
