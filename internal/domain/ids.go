// Package domain holds the fabric's identities, version counters, state
// dimensions and the typed commands and events the reducer speaks.
//
// Nothing here touches a database, a process or a provider. The package is the
// vocabulary shared by the pure reducer (internal/reducer) and the Postgres
// store (internal/store).
package domain

import (
	"fmt"
	"regexp"
	"strings"
)

// Identifiers are opaque strings with a kind prefix. Parsers are strict: a
// value that crosses a boundary (API, database, host message) is parsed, never
// trusted by type alone (DESIGN.md R25).
type (
	MissionID string
	AttemptID string
	EffectID  string
	HostID    string
	ActorID   string
	CommandID string
)

const maxIDLen = 64

var idBody = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,62}$`)

func parseID(prefix, s string) (string, error) {
	if len(s) > maxIDLen {
		return "", fmt.Errorf("%w: id longer than %d bytes", ErrInvalidID, maxIDLen)
	}
	if len(s) <= len(prefix) || s[:len(prefix)] != prefix {
		return "", fmt.Errorf("%w: expected prefix %q", ErrInvalidID, prefix)
	}
	if !idBody.MatchString(s[len(prefix):]) {
		return "", fmt.Errorf("%w: bad characters after %q", ErrInvalidID, prefix)
	}
	return s, nil
}

func ParseMissionID(s string) (MissionID, error) {
	v, err := parseID("m_", s)
	return MissionID(v), err
}
func ParseAttemptID(s string) (AttemptID, error) {
	v, err := parseID("r_", s)
	return AttemptID(v), err
}
func ParseEffectID(s string) (EffectID, error) { v, err := parseID("e_", s); return EffectID(v), err }
func ParseHostID(s string) (HostID, error)     { v, err := parseID("h_", s); return HostID(v), err }

// ParseActorID accepts any bounded token: actors are the daemon, a host agent,
// a worker run or the main chat, each with its own prefix convention.
func ParseActorID(s string) (ActorID, error) {
	if len(s) == 0 || len(s) > maxIDLen || !idBody.MatchString(s) {
		return "", fmt.Errorf("%w: actor", ErrInvalidID)
	}
	return ActorID(s), nil
}

// IsOperator reports whether an actor names a human operator: "op:<name>",
// set with FABRIC_ACTOR. The fab CLI default "chat" is not one, so clearing
// a park or a pause needs a deliberate, attributed actor (main decision on
// red-team N3, goodqa#15082).
// It is an allowlist, so a new automation actor is never an operator.
// Actors are self-asserted by the caller, so this separates code paths
// (fabd, native-runner, import, asks), not authenticated people.
func IsOperator(a ActorID) bool {
	return strings.HasPrefix(string(a), "op:") && len(a) > len("op:")
}

// ParseCommandID accepts the client-minted idempotency key for one command.
func ParseCommandID(s string) (CommandID, error) {
	if len(s) == 0 || len(s) > maxIDLen || !idBody.MatchString(s) {
		return "", fmt.Errorf("%w: command id", ErrInvalidID)
	}
	return CommandID(s), nil
}
