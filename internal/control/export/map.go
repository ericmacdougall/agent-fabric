// Package export turns the fabric outbox into mc.event/1 lines for Mission
// Control (goodqa#15586 slice B2, consumer goodqa#15585). It is read-only and
// content-free by construction: the query never selects an event payload, and a
// summary is built from a fixed phrase table, the mission id and the sequence
// number, so there is no payload text to leak.
package export

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"fabric/internal/domain"
)

// Schema is the pinned event schema: agent-harness d825d66,
// docs/schemas/mc-event-v1.schema.json (blob d7506f0d224dd83d9105ef2f8f443f5fe5efb86a).
const Schema = "mc.event/1"

// Source is the producer name and the id prefix ("fabric:<outbox id>").
const Source = "fabric"

// MaxSummary is the schema's summary limit, in characters.
const MaxSummary = 280

// Row is what the exporter reads for one outbox row. It carries no payload.
type Row struct {
	ID      int64 // outbox id
	Mission string
	Seq     int64
	Kind    string // events.kind; "" when the event row is missing
	At      time.Time
	Held    bool   // the mission's stream is held at or before this row
	Ticket  string // missions.ticket as stored (free text); only a "name#N" reference leaves this package
}

// Event is one mc.event/1 line.
type Event struct {
	Schema   string  `json:"schema"`
	ID       string  `json:"id"`
	TS       string  `json:"ts"`
	Kind     string  `json:"kind"`
	Source   string  `json:"source"`
	Subject  Subject `json:"subject"`
	Summary  string  `json:"summary"`
	Severity string  `json:"severity,omitempty"`
	Historic bool    `json:"historic,omitempty"`
}

// Subject names the stream: the initiative is the fabric mission.
type Subject struct {
	Initiative string `json:"initiative,omitempty"`
	Ticket     string `json:"ticket,omitempty"`
}

var idShape = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,119}$`)

// mcKind maps a known fabric event kind to an mc.event/1 kind. Anything not
// listed (a known kind without a better home) is an owner state change.
var mcKind = map[domain.EventKind]string{
	domain.EvDelivered:         "release",
	domain.EvLiveVerified:      "deploy",
	domain.EvLadderReceipt:     "system",
	domain.EvLadderTopApproved: "system",
	domain.EvDispatchDeferred:  "system",
	domain.EvWakeRecorded:      "system",
	domain.EvWakeAcked:         "system",
}

// warnKinds are the known kinds an operator wants flagged.
var warnKinds = map[domain.EventKind]bool{
	domain.EvAttemptUnknown: true, domain.EvEffectUnknown: true, domain.EvAcceptanceRevoked: true,
	domain.EvClaimRefused: true, domain.EvMissionParked: true,
}

// MapRow maps one outbox row. A held stream or a kind this build does not know
// becomes kind "system" with a fixed summary: never dropped, never decoded.
func MapRow(r Row) Event {
	mission := r.Mission
	if !idShape.MatchString(mission) {
		// A stable, distinct stand-in: malformed ids neither collide with each other nor with a real mission.
		sum := sha256.Sum256([]byte(mission))
		mission = "invalid-" + hex.EncodeToString(sum[:4])
	}
	ev := Event{
		Schema:  Schema,
		ID:      Source + ":" + itoa(r.ID),
		TS:      r.At.UTC().Format("2006-01-02T15:04:05.000Z"),
		Source:  Source,
		Subject: Subject{Initiative: mission, Ticket: ticketRef(r.Ticket)},
	}
	k := domain.EventKind(r.Kind)
	prefix := "mission " + mission + ": "
	suffix := " (seq " + itoa(r.Seq) + ")"
	switch {
	case r.Held:
		ev.Kind, ev.Severity = "system", "warn"
		ev.Summary = prefix + "stream held, event withheld" + suffix
	case r.Kind == "":
		ev.Kind = "system"
		ev.Summary = prefix + "outbox row has no event record" + suffix
	case !domain.KnownEventKind(k):
		ev.Kind = "system"
		ev.Summary = prefix + "event of an unrecognized kind withheld" + suffix
	default:
		ev.Kind = "owner"
		if m, ok := mcKind[k]; ok {
			ev.Kind = m
		}
		if warnKinds[k] {
			ev.Severity = "warn"
		}
		// k is a member of the fixed known-kind set: [A-Z_] only, never stored text.
		ev.Summary = prefix + strings.ToLower(strings.ReplaceAll(r.Kind, "_", " ")) + suffix
	}
	ev.Summary = clip(ev.Summary, MaxSummary)
	return ev
}

func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

var ticketShape = regexp.MustCompile(`[a-z][a-z0-9_-]{0,30}#[0-9]{1,9}`)

// ticketRef extracts the first "project#N" reference (lower-cased, e.g.
// "goodqa#15586") from a mission's ticket text, the join key Mission Control
// derives initiatives from. Nothing else of the text is exported; no match, no field.
func ticketRef(s string) string {
	return ticketShape.FindString(strings.ToLower(s))
}

// MapLag is the system event that tells a file-drop reader (which never sees
// stderr) that committed rows are held back: one per lag episode, keyed by the
// first held outbox id, so replays dedupe on it. The next normal event clears it.
func MapLag(n, firstHeldID int64, at time.Time) Event {
	return Event{
		Schema:   Schema,
		ID:       Source + ":lag:" + itoa(firstHeldID),
		TS:       at.UTC().Format("2006-01-02T15:04:05.000Z"),
		Kind:     "system",
		Source:   Source,
		Summary:  "fabric export lagging: " + itoa(n) + " rows held back",
		Severity: "warn",
	}
}
