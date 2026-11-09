package envelope

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"fabric/internal/domain"
)

// This file is the v2 reader of F1a (docs/adr/0001-kernel-scope.md choice 1
// and 2, goodqa#15372): every stored v1 event is read through View, built by
// Upcast from the events row alone. Nothing here writes: the schema, the
// stored bytes and everything fabd writes stay as they are.

// Reader errors. A decode failure of any of these kinds holds the mission
// (docs/envelope.md); a canonicalisation failure never does.
var (
	ErrUnknownKind = errors.New("envelope: event kind is not in the registry")
	ErrPayload     = errors.New("envelope: payload does not decode as its kind")
	ErrRowMismatch = errors.New("envelope: row and payload disagree on kind, mission or seq")
)

// PersonalTenant is the tenant every pre-F3 event belongs to (ADR 0001
// choice 3). It is a literal, fixed here and in the ADR.
const PersonalTenant = "t_personal"

// LegacyIssuer marks a principal taken from the self-asserted actor column.
// It never carries authority: operator checks keep reading the actor.
const LegacyIssuer = "legacy-unverified"

const eventIDPrefix = "evt_"

// eventIDRE is the shape of an event id.
var eventIDRE = regexp.MustCompile(`^evt_[0-7][0-9A-HJKMNP-TV-Z]{25}$`)

// ValidEventID reports whether s has the event id shape.
func ValidEventID(s string) bool { return eventIDRE.MatchString(s) }

// Row is the part of an events row the upcaster reads. The outbox path joins
// events on (mission_id, seq) so both paths build the same View.
type Row struct {
	Mission string
	Seq     int64
	Kind    string
	Actor   string
	At      time.Time
	Payload []byte
}

// Principal is who an event is attributed to.
type Principal struct {
	Issuer  string `json:"issuer"`
	Subject string `json:"subject"`
}

// View is the v2 envelope of one stored event. Event is the decoded v1
// payload the reducer folds; the rest is the envelope.
type View struct {
	SchemaVersion int
	EventID       string
	TenantID      string
	Principal     Principal
	Mission       string
	Seq           int64
	Kind          string
	At            time.Time
	Event         domain.Event

	payload []byte
}

// DecodeEvent is the one decode function for a stored event payload. Unknown
// keys never fail; a value of the wrong type does. Every event decode site
// uses it (TestNoDirectEventDecode scans for a direct decode elsewhere).
func DecodeEvent(payload []byte) (domain.Event, error) {
	var e domain.Event
	if err := json.Unmarshal(payload, &e); err != nil {
		return domain.Event{}, fmt.Errorf("%w: %v", ErrPayload, err)
	}
	return e, nil
}

// Upcast builds the v2 view of a stored row. It fails with ErrPayload,
// ErrUnknownKind or ErrRowMismatch; it never computes canonical bytes.
func Upcast(r Row) (View, error) {
	e, err := DecodeEvent(r.Payload)
	if err != nil {
		return View{}, err
	}
	if !domain.KnownEventKind(e.Kind) {
		return View{}, fmt.Errorf("%w: %q", ErrUnknownKind, string(e.Kind))
	}
	if string(e.Kind) != r.Kind || string(e.Mission) != r.Mission || (e.Seq != 0 && e.Seq != r.Seq) {
		return View{}, ErrRowMismatch
	}
	if e.Seq == 0 {
		e.Seq = r.Seq
	}
	id, err := EventID(r.Mission, r.Seq, r.At)
	if err != nil {
		return View{}, err
	}
	return View{
		SchemaVersion: 2, EventID: id, TenantID: PersonalTenant,
		Principal: Principal{Issuer: LegacyIssuer, Subject: r.Actor},
		Mission:   r.Mission, Seq: r.Seq, Kind: r.Kind, At: r.At.UTC(), Event: e,
		payload: append([]byte(nil), r.Payload...),
	}, nil
}

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// EventID is evt_ plus a ULID: the time is floor(epoch milliseconds of at),
// the body is the first 80 bits of SHA-256 over "fabric.v1.event_id", 0x00,
// u32be(len(mission)), mission, u64be(seq). It is never an ordering key (seq
// is). A mission that is empty, not ASCII or over 64 bytes is refused.
func EventID(mission string, seq int64, at time.Time) (string, error) {
	if mission == "" || len(mission) > 64 {
		return "", fmt.Errorf("%w: mission length", ErrRowMismatch)
	}
	for i := 0; i < len(mission); i++ {
		if mission[i] >= 0x80 {
			return "", fmt.Errorf("%w: mission is not ASCII", ErrRowMismatch)
		}
	}
	if seq < 0 {
		return "", fmt.Errorf("%w: seq", ErrRowMismatch)
	}
	ms := at.UnixMilli()
	if ms < 0 || ms >= 1<<48 {
		return "", fmt.Errorf("%w: time out of range", ErrRowMismatch)
	}
	h := sha256.New()
	h.Write([]byte("fabric.v1.event_id"))
	h.Write([]byte{0})
	var n4 [4]byte
	binary.BigEndian.PutUint32(n4[:], uint32(len(mission)))
	h.Write(n4[:])
	h.Write([]byte(mission))
	var n8 [8]byte
	binary.BigEndian.PutUint64(n8[:], uint64(seq))
	h.Write(n8[:])
	body := h.Sum(nil)[:10]
	var raw [16]byte
	raw[0], raw[1], raw[2] = byte(ms>>40), byte(ms>>32), byte(ms>>24)
	raw[3], raw[4], raw[5] = byte(ms>>16), byte(ms>>8), byte(ms)
	copy(raw[6:], body)
	// 128 bits as 26 base32 characters: the first carries 3 bits.
	out := make([]byte, 0, len(eventIDPrefix)+26)
	out = append(out, eventIDPrefix...)
	var acc uint32
	bits := 2 // two leading zero bits so the 128 bits fill 26 groups of 5
	for _, b := range raw {
		acc = acc<<8 | uint32(b)
		bits += 8
		for bits >= 5 {
			bits -= 5
			out = append(out, crockford[(acc>>uint(bits))&31])
		}
	}
	return string(out), nil
}

// Canonical returns the RFC 8785 bytes of the view: schema version, event
// id, tenant, principal, mission, seq, kind, at (UTC, six fractional digits,
// Z) and the stored payload. It is computed on demand and never on the replay
// or consumer paths; a failure here is never a hold.
func (v View) Canonical() ([]byte, error) {
	var payload json.RawMessage = v.payload
	doc := struct {
		SchemaVersion int             `json:"schema_version"`
		EventID       string          `json:"event_id"`
		TenantID      string          `json:"tenant_id"`
		Principal     Principal       `json:"principal"`
		Mission       string          `json:"mission"`
		Seq           int64           `json:"seq"`
		Kind          string          `json:"kind"`
		At            string          `json:"at"`
		Payload       json.RawMessage `json:"payload"`
	}{v.SchemaVersion, v.EventID, v.TenantID, v.Principal, v.Mission, v.Seq, v.Kind, AtString(v.At), payload}
	b, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	return Canonicalize(b)
}

// AtString renders a time as UTC with six fractional digits and a Z.
func AtString(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000Z") }
