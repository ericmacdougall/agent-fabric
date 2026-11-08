package host

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"

	"fabric/internal/egress"
)

var linuxProxyEnvironmentNames = managerProxyEnvNames

type linuxNetworkRules struct {
	proxyEnv   []string
	tcpPorts   []int
	properties []string
}

// linuxNetworkPlan compiles the worker half of a PolicyDescriptor. Landlock
// supplies the effective-port rule and systemd's cgroup IP filter supplies the
// numeric-address rule; neither mechanism alone expresses their intersection.
// MediatorRules are validated but never copied into the worker's allow-list:
// the daemon-owned mediator dials those pinned destinations itself.
func linuxNetworkPlan(legacyPorts []int, policy *egress.PolicyDescriptor) (linuxNetworkRules, error) {
	plan := linuxNetworkRules{
		tcpPorts: append([]int(nil), legacyPorts...),
		properties: []string{
			"IPAddressDeny=any",
			"IPAddressAllow=127.0.0.1/32",
		},
	}
	if policy == nil {
		return plan, nil
	}
	if len(legacyPorts) != 0 {
		return plan, errors.New("linux egress: direct TCP ports cannot be combined with a per-run proxy")
	}
	if policy.Version != 1 || !policy.DefaultDeny {
		return plan, errors.New("linux egress: policy must be version 1 and default-deny")
	}
	proxy, err := url.Parse(policy.ProxyURL)
	if err != nil || proxy.Scheme != "http" || proxy.Hostname() != "127.0.0.1" || proxy.User != nil ||
		proxy.Path != "" || proxy.RawQuery != "" || proxy.Fragment != "" || proxy.ForceQuery {
		return plan, errors.New("linux egress: proxy must be a plain http://127.0.0.1:port URL")
	}
	port, err := strconv.ParseUint(proxy.Port(), 10, 16)
	if err != nil || port == 0 || net.JoinHostPort(proxy.Hostname(), proxy.Port()) != proxy.Host {
		return plan, errors.New("linux egress: proxy has an invalid effective port")
	}
	if len(policy.WorkerRules) != 1 {
		return plan, errors.New("linux egress: policy must contain exactly one worker rule")
	}
	worker := policy.WorkerRules[0]
	if worker.Action != "allow" || worker.Protocol != "tcp" || worker.Address != "127.0.0.1" ||
		worker.Port != uint16(port) || worker.Host != "" {
		return plan, errors.New("linux egress: worker rule does not exactly match the loopback proxy")
	}
	if len(policy.MediatorRules) == 0 {
		return plan, errors.New("linux egress: mediator has no pinned provider destination")
	}
	for _, rule := range policy.MediatorRules {
		ip, ipErr := netip.ParseAddr(rule.Address)
		if rule.Action != "allow" || rule.Protocol != "tcp" || rule.Port == 0 || rule.Host == "" || ipErr != nil || !ip.IsValid() {
			return plan, fmt.Errorf("linux egress: invalid mediator rule for %q", rule.Host)
		}
	}
	plan.tcpPorts = []int{int(port)}
	plan.proxyEnv = []string{
		"HTTP_PROXY=" + policy.ProxyURL,
		"HTTPS_PROXY=" + policy.ProxyURL,
		"ALL_PROXY=" + policy.ProxyURL,
		"NO_PROXY=",
		"http_proxy=" + policy.ProxyURL,
		"https_proxy=" + policy.ProxyURL,
		"all_proxy=" + policy.ProxyURL,
		"no_proxy=",
	}
	return plan, nil
}
