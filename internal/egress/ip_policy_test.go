package egress

import (
	"net/netip"
	"testing"
)

func TestPublicInternetUnicastAllowsOrdinaryAddresses(t *testing.T) {
	allowed := []string{
		"1.1.1.1",
		"8.8.8.8",
		"100.63.255.255", // immediately below CGNAT
		"100.128.0.0",    // immediately above CGNAT
		"172.15.255.255",
		"172.32.0.0",
		"192.0.1.255",
		"192.0.3.0",
		"198.17.255.255", // immediately below benchmarking
		"198.20.0.0",     // immediately above benchmarking
		"203.0.112.255",
		"203.0.114.0",
		"223.255.255.255",
		"2001:200::", // immediately above the IETF assignment /23
		"2001:4860:4860::8888",
		"2001:db7::",
		"2003::",
		"2606:4700:4700::1111",
		"3fff:1000::", // immediately above documentation /20
	}
	for _, text := range allowed {
		t.Run(text, func(t *testing.T) {
			ip := netip.MustParseAddr(text)
			if !isPublicInternetUnicast(ip) {
				t.Fatalf("ordinary public address %s was denied", ip)
			}
			got, err := pinAddresses([]netip.Addr{ip}, false)
			if err != nil || len(got) != 1 || got[0] != ip {
				t.Fatalf("pinAddresses(%s) = %v, %v", ip, got, err)
			}
		})
	}
}

func TestSpecialUseAddressesDeniedEvenWithLoopbackOptIn(t *testing.T) {
	denied := []string{
		// IPv4 special-purpose registry and both ends of bounded ranges.
		"0.0.0.0",
		"0.255.255.255",
		"10.0.0.1",
		"100.64.0.0",
		"100.64.0.1",
		"100.127.255.255",
		"169.254.1.1",
		"172.16.0.0",
		"172.31.255.255",
		"192.0.0.0",
		"192.0.0.9",  // globally reachable PCP anycast, still special-use
		"192.0.0.10", // globally reachable TURN anycast, still special-use
		"192.0.2.1",
		"192.31.196.1",
		"192.52.193.1",
		"192.88.99.1",
		"192.168.1.1",
		"192.175.48.1",
		"198.18.0.0",
		"198.19.255.255",
		"198.51.100.1",
		"203.0.113.1",
		"224.0.0.1",
		"239.255.255.255",
		"240.0.0.0",
		"255.255.255.255",

		// IPv6 special-purpose registry, including global-unicast ranges.
		"::",
		"::2", // deprecated IPv4-compatible form
		"64:ff9b::8.8.8.8",
		"64:ff9b:1::",
		"100::",
		"100::ffff:ffff:ffff:ffff",
		"2001::",
		"2001:1ff:ffff::",
		"2001:db8::1",
		"2002::",
		"3fff::",
		"3fff:fff:ffff::",
		"5f00::",
		"4000::", // reserved space outside the IPv6 global-unicast allocation
		"5f01::",
		"7fff:ffff:ffff::",
		"8000::",
		"dfff:ffff:ffff::",
		"e000::",
		"fc00::",
		"fdff:ffff:ffff::",
		"fe80::1",
		"febf:ffff:ffff::",
		"fec0::1",
		"feff:ffff:ffff::",
		"ff02::1",
		"ffff:ffff:ffff::",

		// Mapped and zoned forms must never be normalized into an allow decision.
		"::ffff:8.8.8.8",
		"::ffff:127.0.0.1",
		"2606:4700:4700::1111%eth0",
	}
	for _, text := range denied {
		t.Run(text, func(t *testing.T) {
			ip := netip.MustParseAddr(text)
			if isPublicInternetUnicast(ip) {
				t.Fatalf("special-use address %s passed public policy", ip)
			}
			if got, err := pinAddresses([]netip.Addr{ip}, true); err == nil {
				t.Fatalf("special-use address %s was pinned as %v", ip, got)
			}
		})
	}

	if got, err := pinAddresses([]netip.Addr{{}}, false); err == nil {
		t.Fatalf("invalid zero address was pinned as %v", got)
	}
}

func TestOnlyLoopbackHasTestResolutionException(t *testing.T) {
	for _, text := range []string{"127.0.0.1", "127.0.0.2", "127.255.255.254", "::1"} {
		t.Run(text, func(t *testing.T) {
			ip := netip.MustParseAddr(text)
			if got, err := pinAddresses([]netip.Addr{ip}, false); err == nil {
				t.Fatalf("loopback %s pinned without opt-in as %v", ip, got)
			}
			got, err := pinAddresses([]netip.Addr{ip}, true)
			if err != nil || len(got) != 1 || got[0] != ip {
				t.Fatalf("loopback opt-in for %s = %v, %v", ip, got, err)
			}
		})
	}
}

func TestOneSpecialAnswerFailsWholePinSet(t *testing.T) {
	_, err := pinAddresses([]netip.Addr{
		netip.MustParseAddr("1.1.1.1"),
		netip.MustParseAddr("100.64.0.1"),
		netip.MustParseAddr("2606:4700:4700::1111"),
	}, false)
	if err == nil {
		t.Fatal("a special-use answer did not fail the complete DNS answer set")
	}
}
