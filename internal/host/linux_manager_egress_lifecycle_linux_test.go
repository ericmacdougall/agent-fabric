//go:build linux

package host

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"fabric/internal/egress"
)

type linuxTestResolver struct{ ip netip.Addr }

func (r linuxTestResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return []netip.Addr{r.ip}, nil
}

func TestLinuxMediatorCleanupChecksGenerationAndUnit(t *testing.T) {
	mediator, err := egress.Start(context.Background(), egress.Config{Resolver: linuxTestResolver{ip: netip.MustParseAddr("93.184.216.34")}})
	if err != nil {
		t.Fatal(err)
	}
	h := Handle{Run: "run-current", Token: "secret", PID: 42, Unit: "fab-run-current.service", UnitSerial: 7, CGroup: "/sys/fs/cgroup/current"}
	l := &LinuxManager{mediator: mediator, proxyURL: mediator.URL(), activeSerial: h.UnitSerial, active: h}
	for _, stale := range []Handle{
		func() Handle { h := h; h.UnitSerial--; return h }(),
		func() Handle { h := h; h.Unit = "fab-forged.service"; return h }(),
		func() Handle { h := h; h.CGroup = "/sys/fs/cgroup/forged"; return h }(),
	} {
		l.closeMediator(stale)
		conn, err := net.DialTimeout("tcp", strings.TrimPrefix(mediator.URL(), "http://"), 250*time.Millisecond)
		if err != nil {
			t.Fatalf("stale handle closed the active mediator: %v", err)
		}
		_ = conn.Close()
	}
	l.closeMediator(h)
	if l.mediator != nil || l.proxyURL != "" {
		t.Fatal("verified cleanup retained mediator state")
	}
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(mediator.URL(), "http://"), 250*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		t.Fatal("mediator listener survived verified cleanup")
	}
}
