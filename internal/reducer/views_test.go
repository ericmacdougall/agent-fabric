package reducer

import (
	"fabric/internal/domain"
	"fabric/internal/envelope"
)

// views wraps v1 events as v2 views for tests that fold a log directly.
// Production code builds a View only through envelope.Upcast from a stored
// row; Replay accepts nothing else (the m3 mutant in tools/mutants).
func views(events []domain.Event) []envelope.View {
	out := make([]envelope.View, 0, len(events))
	for _, e := range events {
		out = append(out, envelope.View{SchemaVersion: 2, Mission: string(e.Mission), Seq: e.Seq, Kind: string(e.Kind), Event: e})
	}
	return out
}
