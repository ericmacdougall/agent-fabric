package harness

import (
	"encoding/json"
	"errors"
	"fmt"
)

// CheckStream verifies the stream invariants of one finished session:
// versioned events, gap-free Seq from 1, a stable harness and session, a
// payload that decodes for its kind, exactly one Final and it is last.
// Production tools use it to prove a recorded trace conforms; the conformance
// suite uses the same function.
func CheckStream(evs []Event) error {
	if len(evs) == 0 {
		return errors.New("no events")
	}
	finals := 0
	for i, e := range evs {
		if e.Version != EventVersion {
			return fmt.Errorf("event %d: version %d", i, e.Version)
		}
		if e.Seq != int64(i+1) {
			return fmt.Errorf("event %d: seq %d, want %d (gap or reorder)", i, e.Seq, i+1)
		}
		if e.Harness == "" || e.Session == "" {
			return fmt.Errorf("event %d: missing harness or session", i)
		}
		if e.Harness != evs[0].Harness || e.Session != evs[0].Session {
			return fmt.Errorf("event %d: harness or session changed mid-stream", i)
		}
		var dst any
		switch e.Kind {
		case KindTurn:
			dst = &Turn{}
		case KindToolCall:
			dst = &ToolCall{}
		case KindMessage:
			dst = &Message{}
		case KindUsage:
			dst = &Usage{}
		case KindFinal:
			dst = &Final{}
			finals++
		case KindUnknown:
			dst = &Unknown{}
		default:
			return fmt.Errorf("event %d: unknown kind %q", i, e.Kind)
		}
		if err := json.Unmarshal(e.Payload, dst); err != nil {
			return fmt.Errorf("event %d (%s): payload does not decode: %v", i, e.Kind, err)
		}
	}
	if finals != 1 {
		return fmt.Errorf("%d final events, want exactly 1", finals)
	}
	if evs[len(evs)-1].Kind != KindFinal {
		return errors.New("final is not the last event")
	}
	return nil
}
