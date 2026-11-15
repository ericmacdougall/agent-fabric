package egress

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// goodqa#15966 AC-20: per-run byte and connection counts by destination
// host, with refused CONNECTs counted under a fixed label.
func TestMediatorCountsBytesAndConnectionsPerHost(t *testing.T) {
	target := newFakeTarget(t)
	r := &fakeResolver{ips: []netip.Addr{netip.MustParseAddr("127.0.0.1")}}
	d := &dialRecord{targets: map[string]*fakeTarget{"127.0.0.1:443": target}}
	m := startTestMediator(t, testConfig(r, d))
	hello := len(clientHello("api.anthropic.com"))
	for i := 0; i < 2; i++ {
		c, br := established(t, m, DefaultDestination)
		echo(t, c, br, "0123456789")
		c.Close()
	}
	bad := proxyConn(t, m)
	fmt.Fprintf(bad, "CONNECT evil.example:443 HTTP/1.1\r\nHost: evil.example:443\r\n\r\n")
	buf := make([]byte, 64)
	bad.Read(buf)
	deadline := time.Now().Add(2 * time.Second)
	var got []HostCounts
	for time.Now().Before(deadline) {
		got = m.Counts()
		if len(got) == 2 && got[1].BytesDown >= int64(2*(hello+10)) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(got) != 2 || got[0].Host != RefusedHost || got[0].Refused != 1 || got[0].Connections != 0 {
		t.Fatalf("refused counts: %+v", got)
	}
	api := got[1]
	want := int64(2 * (hello + 10))
	if api.Host != "api.anthropic.com" || api.Connections != 2 || api.BytesUp != want || api.BytesDown != want {
		t.Fatalf("api counts: %+v, want 2 connections and %d bytes each way", api, want)
	}
	for _, h := range got {
		if h.Host == "evil.example" {
			t.Fatal("an attacker-chosen host name was recorded")
		}
	}
}

func TestCountsRecordRoundTrip(t *testing.T) {
	dir := t.TempDir()
	rec := CountsRecord{Mission: "m1", Attempt: "a1", Hosts: []HostCounts{{Host: "api.anthropic.com", Connections: 3, BytesUp: 10, BytesDown: 20}}}
	if err := WriteCounts(dir, rec); err != nil {
		t.Fatal(err)
	}
	if err := WriteCounts(dir, CountsRecord{Mission: "m2", Attempt: "a2"}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(filepath.Join(dir, "a1.egress.json"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode: %v %v", fi, err)
	}
	got, err := ReadCounts(dir, "m1")
	if err != nil || len(got) != 1 || got[0].Hosts[0].Connections != 3 {
		t.Fatalf("read: %+v %v", got, err)
	}
	for _, bad := range []string{"", "../x", ".hidden", "a/b"} {
		if WriteCounts(dir, CountsRecord{Attempt: bad}) == nil {
			t.Fatalf("attempt %q accepted", bad)
		}
	}
}
