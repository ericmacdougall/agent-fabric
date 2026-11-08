package host

import (
	"strings"
	"testing"

	"fabric/internal/egress"
)

func validLinuxEgressPolicy() egress.PolicyDescriptor {
	return egress.PolicyDescriptor{
		Version:     1,
		DefaultDeny: true,
		ProxyURL:    "http://127.0.0.1:18080",
		WorkerRules: []egress.FirewallRule{{Action: "allow", Protocol: "tcp", Address: "127.0.0.1", Port: 18080}},
		MediatorRules: []egress.FirewallRule{{
			Action: "allow", Protocol: "tcp", Address: "192.0.2.10", Port: 443, Host: "api.anthropic.com",
		}},
	}
}

func TestLinuxNetworkPlanUsesOnlyLoopbackProxy(t *testing.T) {
	policy := validLinuxEgressPolicy()
	plan, err := linuxNetworkPlan(nil, &policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.tcpPorts) != 1 || plan.tcpPorts[0] != 18080 {
		t.Fatalf("worker TCP ports = %v, want only proxy port 18080", plan.tcpPorts)
	}
	if len(plan.proxyEnv) != len(linuxProxyEnvironmentNames) {
		t.Fatalf("proxy environment has %d entries", len(plan.proxyEnv))
	}
	for i, name := range linuxProxyEnvironmentNames {
		key, value, ok := strings.Cut(plan.proxyEnv[i], "=")
		if !ok || key != name {
			t.Fatalf("proxy environment entry %d = %q, want %s", i, plan.proxyEnv[i], name)
		}
		if strings.Contains(name, "NO_PROXY") || strings.Contains(name, "no_proxy") {
			if value != "" {
				t.Fatalf("%s permits proxy bypass: %q", name, value)
			}
		} else if value != policy.ProxyURL {
			t.Fatalf("%s = %q, want %q", name, value, policy.ProxyURL)
		}
	}
	joined := strings.Join(plan.properties, " ")
	if joined != "IPAddressDeny=any IPAddressAllow=127.0.0.1/32" {
		t.Fatalf("systemd IP properties = %q", joined)
	}
	for _, entry := range plan.proxyEnv {
		if strings.Contains(entry, "api.anthropic.com") || strings.Contains(entry, "192.0.2.10") {
			t.Fatalf("mediator destination leaked into worker proxy environment: %q", entry)
		}
	}
}

func TestLinuxNetworkPlanRejectsDirectAndHostileRules(t *testing.T) {
	withLegacy := validLinuxEgressPolicy()
	if _, err := linuxNetworkPlan([]int{443}, &withLegacy); err == nil {
		t.Fatal("direct provider port was combined with a per-run proxy")
	}
	cases := []struct {
		name   string
		mutate func(*egress.PolicyDescriptor)
	}{
		{"not default deny", func(p *egress.PolicyDescriptor) { p.DefaultDeny = false }},
		{"wrong version", func(p *egress.PolicyDescriptor) { p.Version = 2 }},
		{"remote worker", func(p *egress.PolicyDescriptor) { p.WorkerRules[0].Address = "192.0.2.10" }},
		{"direct provider port", func(p *egress.PolicyDescriptor) { p.WorkerRules[0].Port = 443 }},
		{"extra worker rule", func(p *egress.PolicyDescriptor) { p.WorkerRules = append(p.WorkerRules, p.WorkerRules[0]) }},
		{"non-loopback proxy", func(p *egress.PolicyDescriptor) { p.ProxyURL = "http://192.0.2.1:18080" }},
		{"proxy hostname", func(p *egress.PolicyDescriptor) { p.ProxyURL = "http://localhost:18080" }},
		{"proxy credentials", func(p *egress.PolicyDescriptor) { p.ProxyURL = "http://user@127.0.0.1:18080" }},
		{"proxy path", func(p *egress.PolicyDescriptor) { p.ProxyURL = "http://127.0.0.1:18080/other" }},
		{"proxy port mismatch", func(p *egress.PolicyDescriptor) { p.ProxyURL = "http://127.0.0.1:18081" }},
		{"no mediator destination", func(p *egress.PolicyDescriptor) { p.MediatorRules = nil }},
		{"mediator DNS address", func(p *egress.PolicyDescriptor) { p.MediatorRules[0].Address = "api.anthropic.com" }},
		{"mediator missing host", func(p *egress.PolicyDescriptor) { p.MediatorRules[0].Host = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy := validLinuxEgressPolicy()
			policy.WorkerRules = append([]egress.FirewallRule(nil), policy.WorkerRules...)
			policy.MediatorRules = append([]egress.FirewallRule(nil), policy.MediatorRules...)
			tc.mutate(&policy)
			if _, err := linuxNetworkPlan(nil, &policy); err == nil {
				t.Fatal("hostile descriptor was accepted")
			}
		})
	}
	legacy, err := linuxNetworkPlan([]int{18081}, nil)
	if err != nil || len(legacy.tcpPorts) != 1 || legacy.tcpPorts[0] != 18081 || len(legacy.proxyEnv) != 0 {
		t.Fatalf("legacy loopback test plan = %+v, err=%v", legacy, err)
	}
}
