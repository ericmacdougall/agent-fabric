package egress

import (
	"net"
	"sort"
	"strconv"
)

// PolicyDescriptor is a host-independent input for Darwin, Linux, or Windows
// firewall compilation. WorkerRules permit only the run's loopback proxy;
// MediatorRules permit the mediator's pinned outbound IP addresses. The worker
// rules must be applied to the worker identity, not to the mediator process.
type PolicyDescriptor struct {
	Version       int            `json:"version"`
	DefaultDeny   bool           `json:"default_deny"`
	ProxyURL      string         `json:"proxy_url"`
	WorkerRules   []FirewallRule `json:"worker_rules"`
	MediatorRules []FirewallRule `json:"mediator_rules"`
}

// FirewallRule is one TCP allow rule. Address is a numeric IP, never a DNS
// name, so firewall compilation cannot introduce a second resolution or a DNS
// rebinding window. Worker loopback rules use the proxy's effective port.
type FirewallRule struct {
	Action   string `json:"action"`
	Protocol string `json:"protocol"`
	Address  string `json:"address"`
	Port     uint16 `json:"port"`
	Host     string `json:"host,omitempty"`
}

func makePolicy(proxyURL string, dests map[string]*destination) PolicyDescriptor {
	_, proxyPortText, _ := net.SplitHostPort(proxyURL[len("http://"):])
	proxyPort, _ := strconv.ParseUint(proxyPortText, 10, 16)
	p := PolicyDescriptor{
		Version:     1,
		DefaultDeny: true,
		ProxyURL:    proxyURL,
		WorkerRules: []FirewallRule{{
			Action: "allow", Protocol: "tcp", Address: "127.0.0.1", Port: uint16(proxyPort),
		}},
	}
	keys := make([]string, 0, len(dests))
	for key := range dests {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		d := dests[key]
		for _, ip := range d.addrs {
			p.MediatorRules = append(p.MediatorRules, FirewallRule{
				Action: "allow", Protocol: "tcp", Address: ip.String(), Port: d.port, Host: d.host,
			})
		}
	}
	return p
}

func clonePolicy(in PolicyDescriptor) PolicyDescriptor {
	out := in
	out.WorkerRules = append([]FirewallRule(nil), in.WorkerRules...)
	out.MediatorRules = append([]FirewallRule(nil), in.MediatorRules...)
	return out
}
