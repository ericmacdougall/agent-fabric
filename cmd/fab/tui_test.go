package main

import (
	"strings"
	"testing"
	"time"

	"fabric/internal/metrics"
)

func env(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }

// fab tui flags: defaults, --keys and colour switches, and refusal of bad
// values with a clear error.
func TestTUIFlags(t *testing.T) {
	f, err := parseTUIFlags(nil, env(nil))
	if err != nil || f.interval != 2*time.Second || f.timeout != time.Second || !f.opts.Color || !f.opts.KeysOn {
		t.Fatalf("defaults: %+v %v", f, err)
	}
	f, err = parseTUIFlags([]string{"--keys=off", "--no-color", "--interval", "500ms"}, env(nil))
	if err != nil || f.opts.KeysOn || f.opts.Color || f.interval != 500*time.Millisecond {
		t.Fatalf("flags: %+v %v", f, err)
	}
	if f, err = parseTUIFlags(nil, env(map[string]string{"NO_COLOR": "1"})); err != nil || f.opts.Color {
		t.Fatalf("NO_COLOR: %+v %v", f, err)
	}
	for _, bad := range [][]string{
		{"--interval", "0"}, {"--interval", "-1s"}, {"--interval", "100ms"},
		{"--timeout", "0"}, {"--timeout", "-1s"}, {"--keys", "maybe"}, {"extra"},
	} {
		_, err := parseTUIFlags(bad, env(nil))
		if err == nil {
			t.Fatalf("%v accepted", bad)
		}
		if !strings.Contains(err.Error(), "tui:") && !strings.Contains(err.Error(), "invalid") {
			t.Fatalf("%v: unclear error %q", bad, err)
		}
	}
}

// fab version prints the same revision fabric_build_info reports, so a
// click-tester can match the deployed binary to the evidence commit.
func TestVersion(t *testing.T) {
	v := buildVersion()
	if v == "" || metrics.Label(v) != v {
		t.Fatalf("version %q would not survive as the fabric_build_info label", v)
	}
	if err := versionMain([]string{"x"}); err == nil {
		t.Fatalf("version accepted an argument")
	}
}
