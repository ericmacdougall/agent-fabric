package egress

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// helloOpts describes a hand-built ClientHello for the SNI tests.
type helloOpts struct {
	names     []string // host_name entries in one server_name extension
	noSNI     bool
	twoSNI    bool // a second server_name extension
	ech       bool
	fragment  bool // split the handshake across two records
	nameType  byte
	trailJunk bool
}

func u16(n int) []byte { return []byte{byte(n >> 8), byte(n)} }

func buildHello(o helloOpts) []byte {
	var body []byte
	body = append(body, 0x03, 0x03)          // legacy_version
	body = append(body, make([]byte, 32)...) // random
	body = append(body, 0)                   // session id
	body = append(body, u16(2)...)           // cipher suites
	body = append(body, 0x13, 0x01)          // TLS_AES_128_GCM_SHA256
	body = append(body, 1, 0)                // compression: null
	var exts []byte
	sni := func() []byte {
		var list []byte
		for _, n := range o.names {
			list = append(list, o.nameType)
			list = append(list, u16(len(n))...)
			list = append(list, n...)
		}
		data := append(u16(len(list)), list...)
		return append(append(u16(extServerName), u16(len(data))...), data...)
	}
	if !o.noSNI {
		exts = append(exts, sni()...)
		if o.twoSNI {
			exts = append(exts, sni()...)
		}
	}
	if o.ech {
		exts = append(exts, append(u16(extEncryptedClientHello), u16(1)...)...)
		exts = append(exts, 0)
	}
	exts = append(exts, 0x00, 0x2b, 0x00, 0x03, 0x02, 0x03, 0x04) // supported_versions: TLS 1.3
	body = append(body, u16(len(exts))...)
	body = append(body, exts...)
	msg := append([]byte{handshakeTypeClientHello, byte(len(body) >> 16), byte(len(body) >> 8), byte(len(body))}, body...)
	if o.trailJunk {
		msg = append(msg, 0xff)
	}
	record := func(p []byte) []byte {
		return append([]byte{recordTypeHandshake, 0x03, 0x01, byte(len(p) >> 8), byte(len(p))}, p...)
	}
	if o.fragment {
		half := len(msg) / 2
		return append(record(msg[:half]), record(msg[half:])...)
	}
	return record(msg)
}

// clientHello is a valid single-record ClientHello naming host; the tunnel
// tests send it first, as every real TLS client does.
func clientHello(host string) []byte { return buildHello(helloOpts{names: []string{host}}) }

func check(b []byte, host string) error {
	return checkClientHello(bufio.NewReaderSize(bytes.NewReader(b), maxClientHelloRecord+64), host)
}

func TestClientHelloServerName(t *testing.T) {
	const host = "api.anthropic.com"
	cases := []struct {
		name string
		in   []byte
		ok   bool
	}{
		{"exact", buildHello(helloOpts{names: []string{host}}), true},
		{"case-insensitive", buildHello(helloOpts{names: []string{"API.Anthropic.COM"}}), true},
		{"other site on the same edge", buildHello(helloOpts{names: []string{"evil.example"}}), false},
		{"suffix trick", buildHello(helloOpts{names: []string{host + ".evil.example"}}), false},
		{"trailing dot", buildHello(helloOpts{names: []string{host + "."}}), false},
		{"no SNI", buildHello(helloOpts{noSNI: true}), false},
		{"two names in one extension", buildHello(helloOpts{names: []string{host, "evil.example"}}), false},
		{"two server_name extensions", buildHello(helloOpts{names: []string{host}, twoSNI: true}), false},
		{"non host_name type", buildHello(helloOpts{names: []string{host}, nameType: 1}), false},
		{"empty name", buildHello(helloOpts{names: []string{""}}), false},
		{"control byte in name", buildHello(helloOpts{names: []string{"api.anthropic.com\n"}}), false},
		{"ECH", buildHello(helloOpts{names: []string{host}, ech: true}), false},
		{"fragmented across records", buildHello(helloOpts{names: []string{host}, fragment: true}), false},
		{"trailing bytes in record", buildHello(helloOpts{names: []string{host}, trailJunk: true}), false},
		{"plain HTTP", []byte("GET / HTTP/1.1\r\nHost: api.anthropic.com\r\n\r\n"), false},
		{"empty", nil, false},
		{"short header", []byte{0x16, 0x03}, false},
		{"oversized record", append([]byte{0x16, 0x03, 0x01, 0x40, 0x01}, make([]byte, 0x4001)...), false},
	}
	for _, c := range cases {
		err := check(c.in, host)
		if (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok=%v", c.name, err, c.ok)
		}
	}
}

// TestClientHelloFromCryptoTLS checks the parser against a real Go TLS client.
func TestClientHelloFromCryptoTLS(t *testing.T) {
	for _, tc := range []struct {
		sni  string
		want string
		ok   bool
	}{{"api.anthropic.com", "api.anthropic.com", true}, {"evil.example", "api.anthropic.com", false}} {
		a, b := net.Pipe()
		go func() {
			c := tls.Client(a, &tls.Config{ServerName: tc.sni, MinVersion: tls.VersionTLS12})
			c.SetDeadline(time.Now().Add(2 * time.Second))
			c.Handshake() //nolint:errcheck // the peer closes after reading the hello
			a.Close()
		}()
		b.SetDeadline(time.Now().Add(2 * time.Second))
		err := checkClientHello(bufio.NewReaderSize(b, maxClientHelloRecord+64), tc.want)
		b.Close()
		if (err == nil) != tc.ok {
			t.Errorf("crypto/tls SNI %q against %q: err = %v, want ok=%v", tc.sni, tc.want, err, tc.ok)
		}
	}
}

// TestTunnelRefusesWrongSNIWithoutRelaying is the end-to-end H1 control: a
// tunnel to an allowed destination whose ClientHello names another site, has
// no SNI, or is not TLS relays zero bytes to the target.
func TestTunnelRefusesWrongSNIWithoutRelaying(t *testing.T) {
	for _, first := range [][]byte{
		buildHello(helloOpts{names: []string{"evil.example"}}),
		buildHello(helloOpts{noSNI: true}),
		[]byte("GET / HTTP/1.1\r\nHost: evil.example\r\n\r\n"),
	} {
		target := newFakeTarget(t)
		r := &fakeResolver{ips: []netip.Addr{netip.MustParseAddr("127.0.0.1")}}
		d := &dialRecord{targets: map[string]*fakeTarget{"127.0.0.1:443": target}}
		m := startTestMediator(t, testConfig(r, d))
		c := proxyConn(t, m)
		fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", DefaultDestination, DefaultDestination)
		br := bufio.NewReader(c)
		line, err := br.ReadString('\n')
		if err != nil || !strings.Contains(line, "200") {
			t.Fatalf("CONNECT reply %q, %v", line, err)
		}
		br.ReadString('\n') //nolint:errcheck // blank line after the status
		c.Write(first)      //nolint:errcheck
		echoed, _ := io.ReadAll(br)
		if len(echoed) != 0 {
			t.Fatalf("first bytes %q: %d bytes came back through the tunnel", first[:8], len(echoed))
		}
	}
}

// TestHelloLengthHelper keeps the byte-bound test honest: the hello it adds is
// a real record whose length field matches its body.
func TestHelloLengthHelper(t *testing.T) {
	h := clientHello("api.anthropic.com")
	if int(binary.BigEndian.Uint16(h[3:5])) != len(h)-5 {
		t.Fatal("clientHello record length is wrong")
	}
}
