package egress

import (
	"bufio"
	"encoding/binary"
	"errors"
	"strings"
)

// maxClientHelloRecord is the largest TLS plaintext record (RFC 8446 5.1).
// The whole ClientHello must arrive in the first record; a client that
// fragments it across records is refused rather than reassembled.
const maxClientHelloRecord = 1 << 14

const (
	extServerName            = 0x0000
	extEncryptedClientHello  = 0xfe0d
	recordTypeHandshake      = 0x16
	handshakeTypeClientHello = 0x01
	serverNameTypeHostName   = 0x00
)

var errNotClientHello = errors.New("egress: first tunnel bytes are not a single-record TLS ClientHello")

// checkClientHello peeks at the first TLS record the client sends through an
// established tunnel and requires a ClientHello whose only server_name is
// exactly host (ASCII case-insensitive). Pinning authorizes an IP, not a
// name; on a shared CDN edge the SNI picks the site, so it must match the
// CONNECT authority (red-team H1, goodqa#15235). Encrypted Client Hello is
// refused because its outer SNI need not name the real site. Nothing is
// consumed: the peeked bytes stay in br for the relay.
//
// Residual, by design of a non-intercepting proxy: TLS hides the HTTP Host
// header and credentials, so a worker can still use its own API key on an
// allowed provider as a channel (goodqa#15142, egress DLP).
func checkClientHello(br *bufio.Reader, host string) error {
	head, err := br.Peek(5)
	if err != nil {
		return errNotClientHello
	}
	if head[0] != recordTypeHandshake || head[1] != 0x03 {
		return errNotClientHello
	}
	n := int(binary.BigEndian.Uint16(head[3:5]))
	if n < 4 || n > maxClientHelloRecord {
		return errNotClientHello
	}
	rec, err := br.Peek(5 + n)
	if err != nil {
		return errNotClientHello
	}
	name, err := serverName(rec[5:])
	if err != nil {
		return err
	}
	if !strings.EqualFold(name, host) {
		return errors.New("egress: TLS server_name does not match the CONNECT destination")
	}
	return nil
}

// serverName parses one complete ClientHello handshake message and returns its
// single host_name server_name.
func serverName(msg []byte) (string, error) {
	if len(msg) < 4 || msg[0] != handshakeTypeClientHello {
		return "", errNotClientHello
	}
	hlen := int(msg[1])<<16 | int(msg[2])<<8 | int(msg[3])
	if hlen != len(msg)-4 {
		return "", errNotClientHello // fragmented, or trailing bytes in the record
	}
	p := &parser{b: msg[4:]}
	p.skip(2 + 32) // legacy_version, random
	p.skip(int(p.u8()))
	p.skip(int(p.u16()))
	p.skip(int(p.u8()))
	if p.err != nil {
		return "", errNotClientHello
	}
	if len(p.b) == 0 {
		return "", errors.New("egress: ClientHello has no server_name")
	}
	exts := &parser{b: p.bytes(int(p.u16()))}
	if p.err != nil || len(p.b) != 0 {
		return "", errNotClientHello
	}
	seen := map[uint16]bool{}
	name := ""
	for len(exts.b) > 0 {
		typ := exts.u16()
		data := exts.bytes(int(exts.u16()))
		if exts.err != nil {
			return "", errNotClientHello
		}
		if seen[typ] {
			return "", errors.New("egress: ClientHello repeats an extension")
		}
		seen[typ] = true
		switch typ {
		case extEncryptedClientHello:
			return "", errors.New("egress: Encrypted Client Hello is refused")
		case extServerName:
			list := &parser{b: data}
			entries := &parser{b: list.bytes(int(list.u16()))}
			if list.err != nil || len(list.b) != 0 {
				return "", errNotClientHello
			}
			kind := entries.u8()
			host := entries.bytes(int(entries.u16()))
			if entries.err != nil || len(entries.b) != 0 || kind != serverNameTypeHostName || len(host) == 0 {
				return "", errors.New("egress: ClientHello needs exactly one host_name server_name")
			}
			for _, c := range host {
				if c <= 0x20 || c >= 0x7f {
					return "", errors.New("egress: server_name is not a plain ASCII host name")
				}
			}
			name = string(host)
		}
	}
	if name == "" {
		return "", errors.New("egress: ClientHello has no server_name")
	}
	return name, nil
}

// parser is a bounds-checked big-endian reader; the first short read sets err
// and every later call returns zero values.
type parser struct {
	b   []byte
	err error
}

func (p *parser) bytes(n int) []byte {
	if p.err != nil || n < 0 || n > len(p.b) {
		p.err = errNotClientHello
		return nil
	}
	out := p.b[:n]
	p.b = p.b[n:]
	return out
}

func (p *parser) skip(n int) { p.bytes(n) }

func (p *parser) u8() uint8 {
	b := p.bytes(1)
	if b == nil {
		return 0
	}
	return b[0]
}

func (p *parser) u16() uint16 {
	b := p.bytes(2)
	if b == nil {
		return 0
	}
	return binary.BigEndian.Uint16(b)
}
