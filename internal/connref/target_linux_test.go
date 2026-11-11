//go:build linux

package connref

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestDeclaredTargetPrecedesActualClientStartup(t *testing.T) {
	for _, suffix := range []string{"", "_FILE", "_FILE_KEY", "_PASSWORD_FILE", "_DATABASE"} {
		unsetEnv(t, "FABRIC_TEST_DSN"+suffix)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("create synthetic endpoint")
	}
	defer listener.Close()
	type observed struct {
		database string
		user     string
		password bool
		err      error
	}
	startup := make(chan observed, 1)
	go func() {
		result := observed{}
		defer func() { startup <- result }()
		conn, e := listener.Accept()
		if e != nil {
			result.err = e
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		header := make([]byte, 4)
		if _, e = io.ReadFull(conn, header); e != nil {
			result.err = e
			return
		}
		n := int(binary.BigEndian.Uint32(header))
		if n < 8 || n > 65536 {
			result.err = io.ErrUnexpectedEOF
			return
		}
		body := make([]byte, n-4)
		if _, e = io.ReadFull(conn, body); e != nil {
			result.err = e
			return
		}
		if binary.BigEndian.Uint32(body[:4]) != 196608 {
			result.err = io.ErrUnexpectedEOF
			return
		}
		parts := splitNull(body[4:])
		for i := 0; i+1 < len(parts); i += 2 {
			if parts[i] == "database" {
				result.database = parts[i+1]
			}
			if parts[i] == "user" {
				result.user = parts[i+1]
			}
		}
		writeFrame := func(kind byte, payload []byte) error {
			packet := make([]byte, 5+len(payload))
			packet[0] = kind
			binary.BigEndian.PutUint32(packet[1:5], uint32(4+len(payload)))
			copy(packet[5:], payload)
			_, err := conn.Write(packet)
			return err
		}
		if e = writeFrame('R', []byte{0, 0, 0, 3}); e != nil {
			result.err = e
			return
		}
		passwordHeader := make([]byte, 5)
		if _, e = io.ReadFull(conn, passwordHeader); e != nil {
			result.err = e
			return
		}
		n = int(binary.BigEndian.Uint32(passwordHeader[1:]))
		if n < 4 || n > 4096 {
			result.err = io.ErrUnexpectedEOF
			return
		}
		password := make([]byte, n-4)
		if _, e = io.ReadFull(conn, password); e != nil {
			result.err = e
			return
		}
		result.password = passwordHeader[0] == 'p' && string(password) == syntheticPassword+"\x00"
		if e = writeFrame('R', []byte{0, 0, 0, 0}); e != nil {
			result.err = e
			return
		}
		for _, parameter := range []string{"server_version\x0016.0\x00", "client_encoding\x00UTF8\x00", "standard_conforming_strings\x00on\x00"} {
			if e = writeFrame('S', []byte(parameter)); e != nil {
				result.err = e
				return
			}
		}
		if e = writeFrame('K', make([]byte, 8)); e != nil {
			result.err = e
			return
		}
		if e = writeFrame('Z', []byte{'I'}); e != nil {
			result.err = e
			return
		}
		// Only startup/authentication is implemented. No query is executed.
		io.Copy(io.Discard, conn)
	}()
	u, _ := url.Parse(fixtureURL())
	u.Host = listener.Addr().String()
	u.Path = "/unapproved_base_fixture"
	t.Setenv("FABRIC_TEST_DSN_FILE", source(t, u.String()))
	t.Setenv("FABRIC_TEST_DSN_DATABASE", "fabric_owned_target")
	r, err := ResolveEnv("FABRIC_TEST_DSN", TestAdmin)
	if err != nil {
		t.Fatal("target reference refused")
	}
	defer r.Close()
	dsn, err := r.DSN()
	if err != nil {
		t.Fatal("target reference unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal("actual pgx client did not complete synthetic startup")
	}
	conn.Close(ctx)
	got := <-startup
	if got.err != nil || !got.password {
		t.Fatal("synthetic startup/authentication failed")
	}
	if got.user != "fixture_admin" {
		t.Fatal("target derivation changed the source user")
	}
	if r.purpose != TestAdmin {
		t.Fatal("target derivation changed purpose")
	}
	if got.database != "fabric_owned_target" {
		t.Fatal("client connected using the source base instead of the declared target")
	}
}

func splitNull(data []byte) []string {
	var out []string
	start := 0
	for i, b := range data {
		if b == 0 {
			out = append(out, string(data[start:i]))
			start = i + 1
		}
	}
	return out
}

func TestTargetRequiresValidReferenceMode(t *testing.T) {
	for _, raw := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "legacy"}[raw], func(t *testing.T) {
			for _, suffix := range []string{"", "_FILE", "_FILE_KEY", "_PASSWORD_FILE", "_DATABASE"} {
				unsetEnv(t, "FABRIC_DSN"+suffix)
			}
			if raw {
				t.Setenv("FABRIC_DSN", fixtureURL())
			}
			t.Setenv("FABRIC_DSN_DATABASE", "fabric_owned_target")
			r, err := ResolveEnv("FABRIC_DSN", App)
			if r != nil {
				r.Close()
			}
			if err == nil {
				t.Fatal("database selector was accepted without a declared reference source")
			}
		})
	}
}
